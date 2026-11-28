package main

import (
	"context"
	"log/slog"

	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
)

// 按人存数据的地方（彻底删账号时逐个删）的名字：每一处在建好时登记（tooling.personData），测试拿这张表对照
// 各存储的表结构里按人存的表，漏登记的会失败。
const (
	dataMemory        = "memory"        // 记忆（internal/notes）
	dataReminders     = "reminders"     // 提醒（internal/reminders）
	dataCalendar      = "calendar"      // 日程（internal/calendar）
	dataResearch      = "research"      // 后台调查（internal/research）
	dataConversations = "conversations" // 对话、背景记录池、图片（internal/conversationstore + 图片文件）
	dataExpenses      = "expenses"      // 账目、小票（internal/expenses + 原图）
)

// conversationEraser 删一个人的对话：先让对话簿忘掉（内存里的不再写盘），再删对话库里的（各条线程、完整记录、背景记录池），
// 最后删图片文件（文件删不掉只记日志：记录已经没了，谁也取不到）。
type conversationEraser struct {
	book   *brain.ConversationBook
	store  *conversationstore.SQLiteStore
	images *attachments.Files // nil = 没有图片
	logger *slog.Logger
}

func (e conversationEraser) ErasePerson(ctx context.Context, person string) error {
	if e.book != nil {
		e.book.Forget(person)
	}
	ids, err := e.store.ErasePerson(ctx, person)
	if err != nil {
		return err
	}
	if e.images == nil {
		return nil
	}
	for _, id := range ids {
		if err := e.images.Remove(id); err != nil && e.logger != nil {
			e.logger.Warn("删图片文件出错（记录已删）", "image", id, "err", err)
		}
	}
	return nil
}
