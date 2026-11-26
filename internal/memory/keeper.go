// Package memory 是长期记忆（v10，2026-11-22 裁定 D1–D6）：记住的事（记事库，记事已并进来）和完整的对话记录都能按意思找回。
//
//   - 由模型调工具写（记住 / 忘掉）；来源（主人让记的 / 模型觉得该记的）由模型判断，代码不看内容（P1）。
//   - 少量常驻的（称呼、忌口这类）每轮附在情况说明里（Brief，不进系统提示词：提示词缓存不受影响）；其余用到时用工具查。
//   - 按意思找：嵌入模型把文字变成向量（Embedder，集群里的嵌入服务），向量存在 SQLite，检索在内存里（Index）。
//     后台把没算过向量的记事和对话轮次补上（Run）。没配嵌入服务时照样能记、列、忘、恢复，「查」只给最近记的。
//   - 只有声纹认出的这句能记、忘、恢复（工具那一层核对；忘掉过确认闸）；一个人只看得到自己的和全家共用的。
package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/notes"
	"github.com/bukahou/haro/internal/telemetry"
)

// Embedder 把文字变成向量（embedding.Client 实现）。
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// Transcripts 是完整的对话记录（conversationstore.SQLiteStore 实现）。
type Transcripts interface {
	UnindexedTurns(ctx context.Context, model string, limit int) ([]conversationstore.TranscriptTurn, error)
	SaveTurnVector(ctx context.Context, person string, seq int64, model, digest string, vector []float32) error
	TurnVectors(ctx context.Context, model string) ([]conversationstore.TurnVector, error)
	TranscriptTurns(ctx context.Context, person string, seqs []int64) ([]conversationstore.TranscriptTurn, error)
}

// 上限与时长（有名字的常量，写明来历；都不开放成配置）。
const (
	// residentMaxItems、residentMaxRunes：常驻记忆每轮最多附几条、一共多少字（每轮都发给模型，多了每轮都多花钱）。
	// 来历：用户 2026-11-26「都按照推荐」（15 条、400 字）
	residentMaxItems = 15
	residentMaxRunes = 400
	// recallNotes、recallTurns：一次查给模型几条记住的事、几轮以前的对话（模型自己判断哪些相关）。定值
	recallNotes = 5
	recallTurns = 3
	// maxContentRunes：记住的一件事最长多少字：一两句话；长篇的材料不是记忆。定值
	maxContentRunes = 500
	// syncEmbedTimeout：记住时当场算向量最多等这么久，等不到就交给后台补（主人不用等）。定值
	syncEmbedTimeout = 3 * time.Second
	// queryEmbedTimeout：查的时候算问句的向量最多等这么久（这一轮主人在等回答）；等不到就只给最近记的。定值：
	// 集群里的服务嵌入一句短话一般几十毫秒
	queryEmbedTimeout = 3 * time.Second
	// indexInterval：后台多久看一次有没有没算过向量的（新的对话轮次）；记住新的一条时马上看。刚说的话本来就在最近几轮里，
	// 晚十几秒能按意思找到不影响；定值
	indexInterval = 20 * time.Second
	// indexBatch、indexBatchesPerRound：后台一次嵌入几条、每一轮最多做几批（补以前的对话时不一口气占满嵌入服务）。定值
	indexBatch           = 16
	indexBatchesPerRound = 50
)

// Keeper 管长期记忆。可以并发使用；Run 之后后台才补向量。
type Keeper struct {
	notes       notes.Repository
	embedder    Embedder    // nil = 没配嵌入服务：不能按意思找
	transcripts Transcripts // nil = 对话记录不能按意思找（没配对话库或嵌入服务）
	index       *Index
	listLimit   int
	logger      *slog.Logger
	wake        chan struct{}
	failing     atomic.Bool // 嵌入服务正连不上（开始失败、恢复时各记一条日志）
}

// NewKeeper 创建长期记忆。embedder、transcripts 可以为 nil（见 Keeper 的字段）；listLimit 是列出时最多给几条。
func NewKeeper(store notes.Repository, embedder Embedder, transcripts Transcripts, listLimit int, logger *slog.Logger) *Keeper {
	if embedder == nil {
		transcripts = nil
	}
	return &Keeper{notes: store, embedder: embedder, transcripts: transcripts, index: NewIndex(), listLimit: listLimit,
		logger: logger.With("component", "memory"), wake: make(chan struct{}, 1)}
}

// SearchesByMeaning 表示能按意思找（配了嵌入服务）。
func (k *Keeper) SearchesByMeaning() bool { return k.embedder != nil }

// Poke 让后台马上看一遍有没有没算过向量的（不阻塞）。
func (k *Keeper) Poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// ErrTooLong：要记的内容太长。
var ErrTooLong = errors.New("要记的内容太长")

// Remember 给 person 记住一件事。配了嵌入服务时当场算向量（等不到就交给后台补）。
func (k *Keeper) Remember(ctx context.Context, person, content string, source notes.Source, resident bool) (notes.Note, error) {
	content = strings.TrimSpace(content)
	if runes := utf8.RuneCountInString(content); runes > maxContentRunes {
		return notes.Note{}, fmt.Errorf("%w（%d 字）：记一两句要点，最多 %d 字", ErrTooLong, runes, maxContentRunes)
	}
	note, err := k.notes.Save(ctx, notes.NewNote{Person: person, Content: content, Source: source, Resident: resident})
	if err != nil {
		return notes.Note{}, err
	}
	k.logger.Info("记住了", "id", note.ID, "person", person, "source", source, "resident", resident)
	if k.embedder != nil {
		embedCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), syncEmbedTimeout)
		defer cancel()
		if err := k.indexNotes(embedCtx, []notes.Note{note}); err != nil {
			k.logger.Warn("当场没算出向量：交给后台补", "id", note.ID, "err", err)
			k.Poke()
		}
	}
	return note, nil
}

// Listing 是一个人记住的事：最近的几条和一共几条，要的时候再加上最近忘掉的。
type Listing struct {
	Notes     []notes.Note
	Total     int
	Forgotten []notes.Note
}

// forgottenListLimit：列出最近忘掉的最多几条（找回用）。定值
const forgottenListLimit = 20

// List 列出 person 看得到的、记住的事（新的在前）；withForgotten 时再列最近忘掉的。
func (k *Keeper) List(ctx context.Context, person string, withForgotten bool) (Listing, error) {
	list, total, err := k.notes.List(ctx, person, k.listLimit)
	if err != nil {
		return Listing{}, err
	}
	listing := Listing{Notes: list, Total: total}
	if withForgotten {
		listing.Forgotten, err = k.notes.Deleted(ctx, person, forgottenListLimit)
	}
	return listing, err
}

// Forget 忘掉一条（软删除，可以恢复）；看不到或已经忘了返回 notes.ErrNotFound。
func (k *Keeper) Forget(ctx context.Context, person string, id int64) (notes.Note, error) {
	note, err := k.notes.Delete(ctx, person, id)
	if err != nil {
		return notes.Note{}, err
	}
	k.index.Remove(KindNote, note.Person, note.ID)
	k.logger.Info("忘掉了", "id", id, "person", person)
	return note, nil
}

// Restore 恢复忘掉的一条；看不到或没忘返回 notes.ErrNotFound。它的向量还在库里：放回索引（没有就交给后台算）。
func (k *Keeper) Restore(ctx context.Context, person string, id int64) (notes.Note, error) {
	note, err := k.notes.Restore(ctx, person, id)
	if err != nil {
		return notes.Note{}, err
	}
	k.logger.Info("恢复了记忆", "id", id, "person", person)
	if k.embedder != nil {
		vector, err := k.notes.Vector(ctx, note.ID, k.embedder.Model())
		switch {
		case err == nil && vector != nil:
			k.index.Put(KindNote, note.Person, note.ID, vector)
		default:
			k.Poke()
		}
	}
	return note, nil
}

// Recollection 是一次「查」的结果：记住的事、以前的对话（最像的在前）。ByMeaning 为 false 时是没能按意思找，
// 只给了最近记的（Note 写明为什么）。
type Recollection struct {
	Notes     []notes.Note
	Turns     []conversationstore.TranscriptTurn
	ByMeaning bool
	Note      string
}

// Recall 按意思找 person 记住的事（自己的和全家共用的）和他以前的对话。没配或连不上嵌入服务时只给最近记的。
func (k *Keeper) Recall(ctx context.Context, person, query string) (Recollection, error) {
	started := time.Now()
	defer func() { recallDuration.Record(ctx, time.Since(started).Seconds()) }()
	if k.embedder == nil {
		return k.recent(ctx, person, "没开按意思找：只给了最近记的几条，以前的对话查不了。")
	}
	vector, err := k.embedQuery(ctx, query)
	if err != nil {
		k.logger.Warn("问句没算出向量：只给最近记的", "err", err)
		return k.recent(ctx, person, "嵌入服务这次连不上：只给了最近记的几条，以前的对话这次查不了。")
	}
	recollection := Recollection{ByMeaning: true}
	var ids []int64
	for _, hit := range k.index.Search(vector, KindNote, []string{person, ""}, recallNotes) {
		ids = append(ids, hit.ID)
	}
	if len(ids) > 0 {
		found, err := k.notes.Get(ctx, person, ids)
		if err != nil {
			return Recollection{}, err
		}
		for _, note := range found {
			if note.DeletedAt.IsZero() { // 索引和库之间刚好有人忘掉了：不给
				recollection.Notes = append(recollection.Notes, note)
			}
		}
	}
	if k.transcripts != nil {
		var seqs []int64
		for _, hit := range k.index.Search(vector, KindTurn, []string{person}, recallTurns) {
			seqs = append(seqs, hit.ID)
		}
		if len(seqs) > 0 {
			if recollection.Turns, err = k.transcripts.TranscriptTurns(ctx, person, seqs); err != nil {
				return Recollection{}, err
			}
		}
	}
	return recollection, nil
}

func (k *Keeper) recent(ctx context.Context, person, why string) (Recollection, error) {
	list, _, err := k.notes.List(ctx, person, k.listLimit)
	return Recollection{Notes: list, Note: why}, err
}

// Brief 实现 brain.Briefer：这个人的（和全家共用的）常驻记忆，每轮附在情况说明里。新的在前，最多 residentMaxItems 条、
// 一共 residentMaxRunes 字（超出的这一轮不附）。读不出来时记警告，这一轮不附（模型照样能用工具查）。
func (k *Keeper) Brief(person string) brain.Briefing {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	resident, err := k.notes.Resident(ctx, person, residentMaxItems)
	if err != nil {
		k.logger.Warn("读常驻记忆失败：这一轮不附", "person", person, "err", err)
		return brain.Briefing{}
	}
	var items []string
	runes := 0
	for _, note := range resident {
		item := fmt.Sprintf("%s（#%d）", note.Content, note.ID)
		if runes += utf8.RuneCountInString(item); runes > residentMaxRunes {
			break
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return brain.Briefing{}
	}
	return brain.Briefing{Lines: []string{"一直记着的事（常驻记忆，括号里是编号；用得上时自然地用，不用特意提起）：" +
		strings.Join(items, "；")}}
}

// storeTimeout：情况说明里读库最多等这么久（库在本机，正常几毫秒）；定值
const storeTimeout = 2 * time.Second

func (k *Keeper) embedQuery(ctx context.Context, query string) ([]float32, error) {
	ctx, cancel := context.WithTimeout(ctx, queryEmbedTimeout)
	defer cancel()
	started := time.Now()
	vector, err := k.embedder.EmbedQuery(ctx, query)
	embedDuration.Record(ctx, time.Since(started).Seconds())
	return vector, err
}

var (
	meter          = otel.Meter("github.com/bukahou/haro/memory")
	embedDuration  = telemetry.MustLatencyHistogram(meter, "haro.memory.embed.duration", "一次嵌入请求（问句或一批内容）用了多久")
	recallDuration = telemetry.MustLatencyHistogram(meter, "haro.memory.recall.duration", "一次按意思查（嵌入问句 + 检索 + 取内容）用了多久")
	indexEntries   = telemetry.MustGauge(meter, "haro.memory.index.entries", "内存里的向量索引有几条（记住的事 + 以前的对话轮次）", "{entry}")
)
