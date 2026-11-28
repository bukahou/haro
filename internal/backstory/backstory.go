// Package backstory 是背景记录池：后台把一个人的对话（无痕的、删了的除外）压成几条「关于主人的背景」—— 近况、偏好、进行中的事、
// 常提的人和地方 —— 每轮接在主人资料后面带给模型，haro 一开口就知道最近的情况（细节仍靠长期记忆按意思查原始记录）。
//
// 一条对话闲下来后（和闲置压缩同一个时机）把新内容并进去，每天再整理一次（合并重复、去掉过时的）；合并、整理都交给代理「背景记录池」，
// 它用工具交回新的条目清单（⛔ 不从它写的文字里找标记）。主人在 App 里改过的条目后台不改、不删。总长度有上限（MaxRunes）。
package backstory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bukahou/haro/internal/conversationstore"
)

// 背景记录池的设置（定值）。
const (
	// MaxRunes 是一个人的背景记录池最多多少字（每轮都带给模型，算输入的钱）。
	MaxRunes = 3000
	// checkEvery 是多久看一次有没有闲下来、要并的人。
	checkEvery = 5 * time.Minute
	// consolidateEvery 是多久整理一次。
	consolidateEvery = 24 * time.Hour
	// mergeBatch 是一次最多并几轮（太久没并的分几次并）。
	mergeBatch = 40
	// mergeTimeout 是一次合并、整理最多等多久。
	mergeTimeout = 2 * time.Minute
)

// Store 是背景记录池的存储（conversationstore.SQLiteStore 实现）。
type Store interface {
	BackstoryOf(ctx context.Context, person string) (conversationstore.Backstory, error)
	BackstoryPeople(ctx context.Context) ([]string, error)
	ReplaceGenerated(ctx context.Context, person string, generated []conversationstore.BackstoryItem, mergedSeq int64,
		consolidated *time.Time, now time.Time) error
	EditBackstoryItem(ctx context.Context, person, id, text string, now time.Time) error
	DeleteBackstoryItem(ctx context.Context, person, id string) error
	SetBackstoryEnabled(ctx context.Context, person string, enabled bool) error
	PeopleToMerge(ctx context.Context, idleBefore time.Time) ([]conversationstore.PendingPerson, error)
	TurnsAfter(ctx context.Context, person string, after int64, limit int) ([]conversationstore.FoundTurn, error)
}

// Entry 是交给代理、代理交回的一条。
type Entry struct {
	Text      string
	SourceSeq int64 // 从哪一轮来（0 = 不知道）
}

// Merger 合并、整理背景记录池（代理「背景记录池」实现，见 AgentMerger）。kept 是主人改过的（只读，不交回）；
// current 是后台生成的；turns 是要并进来的新轮次（整理时为空）。交回新的「后台生成的」清单。
type Merger interface {
	Merge(ctx context.Context, kept, current []Entry, turns []conversationstore.FoundTurn) ([]Entry, error)
}

// Service 是背景记录池。
type Service struct {
	store     Store
	merger    Merger
	idleAfter time.Duration
	logger    *slog.Logger
	now       func() time.Time

	mu     sync.RWMutex
	blocks map[string]string // 人 → 放进提示词的那一段（开着、有条目时）
}

// Open 读出所有人的背景记录池（提示词用的那一段留在内存里）。idleAfter 是一条对话闲多久算闲下来。
func Open(ctx context.Context, store Store, merger Merger, idleAfter time.Duration, logger *slog.Logger) (*Service, error) {
	service := &Service{store: store, merger: merger, idleAfter: idleAfter, logger: logger.With("component", "backstory"),
		now: time.Now, blocks: map[string]string{}}
	people, err := store.BackstoryPeople(ctx)
	if err != nil {
		return nil, err
	}
	for _, person := range people {
		if err := service.refresh(ctx, person); err != nil {
			return nil, err
		}
	}
	return service, nil
}

// refresh 按存着的重新算这个人放进提示词的那一段。
func (s *Service) refresh(ctx context.Context, person string) error {
	backstory, err := s.store.BackstoryOf(ctx, person)
	if err != nil {
		return err
	}
	block := ""
	if backstory.Enabled {
		block = render(backstory.Items)
	}
	s.mu.Lock()
	if block == "" {
		delete(s.blocks, person)
	} else {
		s.blocks[person] = block
	}
	s.mu.Unlock()
	return nil
}

// render 是放进提示词的那一段；超过 MaxRunes 时从最旧的起不放（存着的不动）。
func render(items []conversationstore.BackstoryItem) string {
	var lines []string
	total := 0
	for i := len(items) - 1; i >= 0; i-- {
		line := "- " + items[i].Text
		total += utf8.RuneCountInString(line)
		if total > MaxRunes {
			break
		}
		lines = append([]string{line}, lines...)
	}
	if len(lines) == 0 {
		return ""
	}
	return "【关于主人的背景（后台从以前的对话整理的，可能过时；和主人资料、主人明说要记住的事冲突时，以它们为准）】\n" +
		strings.Join(lines, "\n")
}

// Block 是这个人放进提示词的那一段（没有、关了时为空）。每轮都调用：不读库。
func (s *Service) Block(person string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.blocks[person]
}

// Run 定时并入闲下来的人的新轮次、每天整理一次，随 ctx 结束。
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick 做一轮：并入闲下来的人的新轮次；超过 consolidateEvery 没整理的整理一次。
func (s *Service) Tick(ctx context.Context) {
	people, err := s.store.PeopleToMerge(ctx, s.now().Add(-s.idleAfter))
	if err != nil {
		s.logger.Warn("找要并的人出错（下一轮再试）", "err", err)
		return
	}
	merged := map[string]bool{}
	for _, pending := range people {
		if ctx.Err() != nil {
			return
		}
		if err := s.mergeNew(ctx, pending.Person); err != nil {
			s.logger.Warn("并入背景记录池没成（下一轮再试）", "person", pending.Person, "err", err)
		}
		merged[pending.Person] = true
	}
	known, err := s.store.BackstoryPeople(ctx)
	if err != nil {
		s.logger.Warn("读背景记录池出错", "err", err)
		return
	}
	for _, person := range known {
		if ctx.Err() != nil {
			return
		}
		if err := s.consolidateIfDue(ctx, person); err != nil {
			s.logger.Warn("整理背景记录池没成（下一轮再试）", "person", person, "err", err)
		}
	}
}

// mergeNew 把这个人还没并的轮次（一次最多 mergeBatch 轮）并进去。
func (s *Service) mergeNew(ctx context.Context, person string) error {
	ctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	defer cancel()
	backstory, err := s.store.BackstoryOf(ctx, person)
	if err != nil || !backstory.Enabled {
		return err
	}
	turns, err := s.store.TurnsAfter(ctx, person, backstory.MergedSeq, mergeBatch)
	if err != nil || len(turns) == 0 {
		return err
	}
	kept, current := split(backstory.Items)
	entries, err := s.merger.Merge(ctx, kept, current, turns)
	if err != nil {
		return err
	}
	threads := map[int64]string{}
	for _, turn := range turns {
		threads[turn.Seq] = turn.Thread
	}
	if err := s.save(ctx, person, entries, threads, turns[len(turns)-1].Seq, nil); err != nil {
		return err
	}
	s.logger.Info("背景记录池并入了新的对话", "person", person, "turns", len(turns), "items", len(entries))
	return nil
}

// consolidateIfDue 在超过 consolidateEvery 没整理、有后台生成的条目时整理一次。
func (s *Service) consolidateIfDue(ctx context.Context, person string) error {
	ctx, cancel := context.WithTimeout(ctx, mergeTimeout)
	defer cancel()
	backstory, err := s.store.BackstoryOf(ctx, person)
	if err != nil || !backstory.Enabled {
		return err
	}
	now := s.now()
	if backstory.ConsolidatedAt != nil && now.Sub(*backstory.ConsolidatedAt) < consolidateEvery {
		return nil
	}
	kept, current := split(backstory.Items)
	if len(current) == 0 {
		return nil
	}
	entries, err := s.merger.Merge(ctx, kept, current, nil)
	if err != nil {
		return err
	}
	threads := map[int64]string{}
	for _, item := range backstory.Items {
		threads[item.SourceSeq] = item.SourceThread
	}
	if err := s.save(ctx, person, entries, threads, backstory.MergedSeq, &now); err != nil {
		return err
	}
	s.logger.Info("背景记录池整理了", "person", person, "before", len(current), "after", len(entries))
	return nil
}

// save 存下后台生成的新清单（主人改过的不动），重新算提示词那一段。
func (s *Service) save(ctx context.Context, person string, entries []Entry, threads map[int64]string, mergedSeq int64,
	consolidated *time.Time) error {
	items := make([]conversationstore.BackstoryItem, 0, len(entries))
	for _, entry := range entries {
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			continue
		}
		id, err := randomID()
		if err != nil {
			return err
		}
		item := conversationstore.BackstoryItem{ID: id, Text: text}
		if thread, ok := threads[entry.SourceSeq]; ok && entry.SourceSeq > 0 { // 代理给的编号要是交给它的那几轮里的
			item.SourceThread, item.SourceSeq = thread, entry.SourceSeq
		}
		items = append(items, item)
	}
	if err := s.store.ReplaceGenerated(ctx, person, items, mergedSeq, consolidated, s.now()); err != nil {
		return err
	}
	return s.refresh(ctx, person)
}

// split 分出主人改过的（只读）和后台生成的。
func split(items []conversationstore.BackstoryItem) (kept, current []Entry) {
	for _, item := range items {
		entry := Entry{Text: item.Text, SourceSeq: item.SourceSeq}
		if item.Edited {
			kept = append(kept, entry)
		} else {
			current = append(current, entry)
		}
	}
	return kept, current
}

// View 是 App 里看到的背景记录池：条目、用了多少字。
func (s *Service) View(ctx context.Context, person string) (conversationstore.Backstory, int, error) {
	backstory, err := s.store.BackstoryOf(ctx, person)
	if err != nil {
		return conversationstore.Backstory{}, 0, err
	}
	used := 0
	for _, item := range backstory.Items {
		used += utf8.RuneCountInString(item.Text)
	}
	return backstory, used, nil
}

// Edit 是主人改一条（之后后台不改、不删它）。
func (s *Service) Edit(ctx context.Context, person, id, text string) error {
	if err := s.store.EditBackstoryItem(ctx, person, id, strings.TrimSpace(text), s.now()); err != nil {
		return err
	}
	return s.refresh(ctx, person)
}

// Delete 是主人删一条。
func (s *Service) Delete(ctx context.Context, person, id string) error {
	if err := s.store.DeleteBackstoryItem(ctx, person, id); err != nil {
		return err
	}
	return s.refresh(ctx, person)
}

// SetEnabled 是主人开、关背景记录池（关了就不带、也不更新）。
func (s *Service) SetEnabled(ctx context.Context, person string, enabled bool) error {
	if err := s.store.SetBackstoryEnabled(ctx, person, enabled); err != nil {
		return err
	}
	return s.refresh(ctx, person)
}

func randomID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("编号: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ProfileSource 是 brain.ProfileSource 的样子（主人资料）。
type ProfileSource interface {
	Profile(person string) string
}

// WithProfile 把背景记录池接在主人资料后面（实现 brain.ProfileSource）：都在系统提示词里，很少变，吃得到提示词缓存。
type WithProfile struct {
	Profiles ProfileSource // nil = 没有主人资料
	Pool     *Service
}

// Profile 实现 brain.ProfileSource。
func (w WithProfile) Profile(person string) string {
	var parts []string
	if w.Profiles != nil {
		if profile := w.Profiles.Profile(person); profile != "" {
			parts = append(parts, profile)
		}
	}
	if block := w.Pool.Block(person); block != "" {
		parts = append(parts, block)
	}
	return strings.Join(parts, "\n\n")
}

// DefaultIdle 是没开闲置压缩时，一条对话闲多久算闲下来（并进背景记录池）。
const DefaultIdle = 30 * time.Minute
