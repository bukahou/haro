// Package chat 是 App 的打字对话：一个人有语音那条对话（brain.VoiceThread）和 App 里新建的几条（线程），
// 每说一句就开一个打字会话（brain.Session 的 TextOnly）答完这一句，回答的事件转给 App（Server-Sent Events）。
// App 断开后这一轮照样答完、存好；同一条对话同时只答一句。
package chat

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// 错误（App 的接口按它们给状态，⛔ 不看文字）。
var (
	ErrNotFound      = errors.New("没有这条对话")
	ErrBusy          = errors.New("这条对话上一句还在答")
	ErrVoiceThread   = errors.New("语音那条对话不能删")
	ErrTurnNotFound  = errors.New("这一轮已经答完了或没有")
	errSessionClosed = errors.New("打字会话没给出这一轮的结果就结束了")
)

// Store 是线程的信息和完整记录（conversationstore.SQLiteStore 实现）。
type Store interface {
	CreateThread(ctx context.Context, person, id string, incognito bool, now time.Time) (conversationstore.Thread, error)
	ThreadOf(ctx context.Context, person, id string) (conversationstore.Thread, error)
	Threads(ctx context.Context, person string, withDeleted bool) ([]conversationstore.Thread, error)
	DeleteThread(ctx context.Context, person, id string, now time.Time) error
	RestoreThread(ctx context.Context, person, id string) error
	ThreadTurns(ctx context.Context, person, thread string, before int64, limit int) ([]conversationstore.RecordedTurn, error)
	SetThreadTitle(ctx context.Context, person, id, title string) error
	AddAttachment(ctx context.Context, attachment conversationstore.Attachment) error
	AttachmentOf(ctx context.Context, person, id string) (conversationstore.Attachment, error)
	SearchTurns(ctx context.Context, person, query string, limit int) ([]conversationstore.FoundTurn, error)
	TurnsBySeq(ctx context.Context, person string, seqs []int64) ([]conversationstore.FoundTurn, error)
}

// Images 存、取 App 里发的图片（attachments.Files 实现）。
type Images interface {
	Write(id string, data []byte) error
	Read(id string) ([]byte, error)
}

// Recaller 按意思找这个人以前的对话（长期记忆的向量），给出轮次编号；没开按意思找时给空的。
type Recaller interface {
	RecallTurns(ctx context.Context, person, query string) ([]int64, error)
}

// Options 是打字对话的部件。Titler、Images、Recall 可以是 nil（不起标题 / 不收图片 / 只按文字找）。
type Options struct {
	Store    Store
	Sessions SessionFactory
	Titler   Titler
	Images   Images
	Recall   Recaller
	Logger   *slog.Logger
}

// TextSession 是答一句话的打字会话（brain.Session 实现）。
type TextSession interface {
	SubmitText(input brain.TextInput) error
	Events() <-chan realtimeproto.ServerEvent
	Send(ctx context.Context, event realtimeproto.ClientEvent) error
	Close() error
}

// Speaker 是谁在打字：账号的两个人和是不是管理员（管理员账号的人沿用声纹名，见 brain.Residents）。
type Speaker struct {
	Owner, Guest string
	Admin        bool
}

// SessionFactory 为这个人的一条对话开一个打字会话。
type SessionFactory func(speaker Speaker, thread string) TextSession

// titleTimeout 是起一次标题最多等多久（后台做：起不成就没有标题，下一轮再起）。
const titleTimeout = 30 * time.Second

// Service 是打字对话。
type Service struct {
	lifetime   context.Context // 后台起标题跟着它停
	store      Store
	newSession SessionFactory
	titler     Titler   // nil = 不起标题
	images     Images   // nil = 不收图片
	recall     Recaller // nil = 只按文字找
	logger     *slog.Logger

	mu      sync.Mutex
	running map[turnKey]*Turn // 正在答的（每条对话最多一句）
}

type turnKey struct{ person, thread string }

// NewService 建打字对话；后台起标题随 lifetime 停。
func NewService(lifetime context.Context, opts Options) *Service {
	return &Service{lifetime: lifetime, store: opts.Store, newSession: opts.Sessions, titler: opts.Titler, images: opts.Images,
		recall: opts.Recall, logger: opts.Logger.With("component", "chat"), running: map[turnKey]*Turn{}}
}

// EventType 是回答的事件。
type EventType string

// 回答的事件（依次：开始、若干文字片段、结束或失败）。
const (
	EventStarted EventType = "started"
	EventText    EventType = "text"
	EventDone    EventType = "done"
	EventFailed  EventType = "failed"
)

// Event 是回答的一个事件。
type Event struct {
	Type   EventType
	TurnID string                       // started
	Text   string                       // text
	Status realtimeproto.ResponseStatus // done：completed / incomplete / cancelled
	Reason realtimeproto.ResponseReason // failed、incomplete 的原因
}

// Turn 是正在答的一句。Events 在这一轮结束后关闭；Detach 之后（App 断开）事件丢掉，但这一轮照样答完、存好。
type Turn struct {
	ID       string
	said     string // 主人说的这一句（起标题用）
	events   chan Event
	detached chan struct{}
	detach   sync.Once
	session  TextSession
}

// Events 是这一轮的事件。
func (t *Turn) Events() <-chan Event { return t.events }

// Detach 不再收这一轮的事件（App 断开了）：这一轮照样答完、存好。
func (t *Turn) Detach() { t.detach.Do(func() { close(t.detached) }) }

// CheckThread 核对这条对话是这个人的、没删（语音那条总是有）。
func (s *Service) CheckThread(ctx context.Context, person, thread string) error {
	if thread == brain.VoiceThread {
		return nil
	}
	found, err := s.store.ThreadOf(ctx, person, thread)
	switch {
	case errors.Is(err, conversationstore.ErrThreadNotFound):
		return ErrNotFound
	case err != nil:
		return err
	case found.DeletedAt != nil:
		return ErrNotFound
	}
	return nil
}

// Message 是说的一句：文字，和可选的一张图片（先用 Upload 传上来的附件编号）。
type Message struct {
	Text       string
	Attachment string
}

// Send 在这条对话里说一句：开一个打字会话答它。带的图片要是这条对话里传的。
func (s *Service) Send(ctx context.Context, speaker Speaker, thread string, message Message, language string) (*Turn, error) {
	if err := s.CheckThread(ctx, speaker.Owner, thread); err != nil {
		return nil, err
	}
	input := brain.TextInput{Text: message.Text, Person: speaker.Owner, Language: language}
	if message.Attachment != "" {
		image, err := s.imageIn(ctx, speaker.Owner, thread, message.Attachment)
		if err != nil {
			return nil, err
		}
		input.Image, input.Attachment = image, message.Attachment
	}
	text := message.Text
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	key := turnKey{person: speaker.Owner, thread: thread}
	turn := &Turn{ID: id, said: text, events: make(chan Event, 64), detached: make(chan struct{})}
	s.mu.Lock()
	if _, busy := s.running[key]; busy {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.running[key] = turn
	s.mu.Unlock()
	turn.session = s.newSession(speaker, thread)
	if err := turn.session.SubmitText(input); err != nil {
		s.finish(key, turn)
		return nil, fmt.Errorf("交给打字会话: %w", err)
	}
	go s.pump(key, turn)
	return turn, nil
}

// pump 把打字会话的事件转成回答的事件，直到这一轮结束；结束后关掉会话（Close 等这一轮写进历史）。
func (s *Service) pump(key turnKey, turn *Turn) {
	defer s.finish(key, turn)
	defer close(turn.events)
	turn.emit(Event{Type: EventStarted, TurnID: turn.ID})
	var answer strings.Builder
	for event := range turn.session.Events() {
		switch e := event.(type) {
		case realtimeproto.ResponseOutputAudioTranscriptDelta:
			answer.WriteString(e.Delta)
			turn.emit(Event{Type: EventText, Text: e.Delta})
		case realtimeproto.ResponseDone:
			if e.Status == realtimeproto.StatusFailed {
				turn.emit(Event{Type: EventFailed, Reason: e.Reason})
			} else {
				turn.emit(Event{Type: EventDone, Status: e.Status, Reason: e.Reason})
			}
			if e.Status == realtimeproto.StatusCompleted && key.thread != brain.VoiceThread && s.titler != nil {
				go s.titleThread(key, turn.said, answer.String())
			}
			return
		}
	}
	s.logger.Error("打字会话的事件流断了", "err", errSessionClosed)
	turn.emit(Event{Type: EventFailed})
}

// emit 发一个事件给 App；App 断开后丢掉（不拖住这一轮）。
func (t *Turn) emit(event Event) {
	select {
	case t.events <- event:
	case <-t.detached:
	}
}

func (s *Service) finish(key turnKey, turn *Turn) {
	s.mu.Lock()
	if s.running[key] == turn {
		delete(s.running, key)
	}
	s.mu.Unlock()
	if turn.session != nil {
		if err := turn.session.Close(); err != nil {
			s.logger.Warn("关打字会话出错", "err", err)
		}
	}
}

// titleThread 给还没有标题的对话起标题（第一轮答完后，后台做）。起不成只记日志：下一轮答完再起。
func (s *Service) titleThread(key turnKey, said, answer string) {
	ctx, cancel := context.WithTimeout(s.lifetime, titleTimeout)
	defer cancel()
	thread, err := s.store.ThreadOf(ctx, key.person, key.thread)
	if err != nil || thread.Title != "" {
		return
	}
	title, err := s.titler.Title(ctx, said, answer)
	if err != nil {
		s.logger.Warn("起标题没成（下一轮再起）", "thread", key.thread, "err", err)
		return
	}
	if err := s.store.SetThreadTitle(ctx, key.person, key.thread, title); err != nil {
		s.logger.Warn("存标题没成", "thread", key.thread, "err", err)
	}
}

// Stop 停止正在答的这一轮（已经说出来的留着）。
func (s *Service) Stop(ctx context.Context, person, thread, turnID string) error {
	s.mu.Lock()
	turn, ok := s.running[turnKey{person: person, thread: thread}]
	s.mu.Unlock()
	if !ok || turn.ID != turnID || turn.session == nil {
		return ErrTurnNotFound
	}
	return turn.session.Send(ctx, realtimeproto.ResponseCancel{})
}

// Threads 是这个人的 App 对话（不含删了的，新的在前）。
func (s *Service) Threads(ctx context.Context, person string) ([]conversationstore.Thread, error) {
	return s.store.Threads(ctx, person, false)
}

// Create 新建一条对话。
func (s *Service) Create(ctx context.Context, person string, incognito bool) (conversationstore.Thread, error) {
	id, err := randomID()
	if err != nil {
		return conversationstore.Thread{}, err
	}
	return s.store.CreateThread(ctx, person, id, incognito, time.Now())
}

// Delete 删一条对话（软删除）；语音那条不能删。
func (s *Service) Delete(ctx context.Context, person, thread string) error {
	if thread == brain.VoiceThread {
		return ErrVoiceThread
	}
	if err := s.store.DeleteThread(ctx, person, thread, time.Now()); errors.Is(err, conversationstore.ErrThreadNotFound) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// Restore 恢复删了的对话。
func (s *Service) Restore(ctx context.Context, person, thread string) (conversationstore.Thread, error) {
	if err := s.store.RestoreThread(ctx, person, thread); errors.Is(err, conversationstore.ErrThreadNotFound) {
		return conversationstore.Thread{}, ErrNotFound
	} else if err != nil {
		return conversationstore.Thread{}, err
	}
	return s.store.ThreadOf(ctx, person, thread)
}

// Turns 往前翻这条对话。
func (s *Service) Turns(ctx context.Context, person, thread string, before int64, limit int) ([]conversationstore.RecordedTurn, error) {
	if err := s.CheckThread(ctx, person, thread); err != nil {
		return nil, err
	}
	return s.store.ThreadTurns(ctx, person, thread, before, limit)
}

// ErrNoImages 是没开收图片（没配存图片的目录）。
var ErrNoImages = errors.New("没开收图片")

// Upload 在这条对话里传一张图片：解码、缩到上限以内、存成 JPEG，返回附件编号。
func (s *Service) Upload(ctx context.Context, person, thread string, data []byte) (conversationstore.Attachment, error) {
	if s.images == nil {
		return conversationstore.Attachment{}, ErrNoImages
	}
	if err := s.CheckThread(ctx, person, thread); err != nil {
		return conversationstore.Attachment{}, err
	}
	encoded, width, height, err := attachments.Normalize(data)
	if err != nil {
		return conversationstore.Attachment{}, err
	}
	id, err := randomID()
	if err != nil {
		return conversationstore.Attachment{}, err
	}
	if err := s.images.Write(id, encoded); err != nil {
		return conversationstore.Attachment{}, err
	}
	attachment := conversationstore.Attachment{ID: id, Person: person, Thread: thread, Width: width, Height: height, Bytes: len(encoded),
		CreatedAt: time.Now()}
	if err := s.store.AddAttachment(ctx, attachment); err != nil {
		return conversationstore.Attachment{}, err
	}
	return attachment, nil
}

// Image 取回这个人的一张图片（JPEG）。
func (s *Service) Image(ctx context.Context, person, id string) ([]byte, error) {
	if s.images == nil {
		return nil, ErrNoImages
	}
	if _, err := s.store.AttachmentOf(ctx, person, id); errors.Is(err, conversationstore.ErrAttachmentNotFound) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return s.images.Read(id)
}

// imageIn 取这条对话里传的一张图片（别的对话的、别人的都当成没有）。
func (s *Service) imageIn(ctx context.Context, person, thread, id string) ([]byte, error) {
	if s.images == nil {
		return nil, ErrNoImages
	}
	attachment, err := s.store.AttachmentOf(ctx, person, id)
	switch {
	case errors.Is(err, conversationstore.ErrAttachmentNotFound) || (err == nil && attachment.Thread != thread):
		return nil, ErrAttachmentNotFound
	case err != nil:
		return nil, err
	}
	return s.images.Read(id)
}

// ErrAttachmentNotFound 是说的一句带的图片不是这条对话里传的。
var ErrAttachmentNotFound = errors.New("没有这张图片（要在这条对话里先传）")

// searchLimit 是搜一次最多给几轮。
const searchLimit = 20

// Search 找这个人以前的对话：按文字（子串）和按意思（长期记忆的向量）找到的合在一起，新的在前；删了的对话里的不算。
func (s *Service) Search(ctx context.Context, person, query string) ([]conversationstore.FoundTurn, error) {
	found, err := s.store.SearchTurns(ctx, person, query, searchLimit)
	if err != nil {
		return nil, err
	}
	if s.recall != nil {
		seqs, err := s.recall.RecallTurns(ctx, person, query)
		if err != nil {
			s.logger.Warn("按意思找对话出错：只给按文字找到的", "err", err)
		} else {
			byMeaning, err := s.store.TurnsBySeq(ctx, person, seqs)
			if err != nil {
				return nil, err
			}
			found = append(found, byMeaning...)
		}
	}
	seen := map[int64]bool{}
	merged := found[:0]
	for _, turn := range found {
		if !seen[turn.Seq] {
			seen[turn.Seq] = true
			merged = append(merged, turn)
		}
	}
	slices.SortFunc(merged, func(a, b conversationstore.FoundTurn) int { return cmp.Compare(b.Seq, a.Seq) })
	return merged[:min(len(merged), searchLimit)], nil
}

// randomID 是随机编号（128 位，十六进制）。
func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
