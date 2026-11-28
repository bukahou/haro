package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/endpoint"
	"github.com/bukahou/haro/internal/realtimeproto"
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
	dataVoiceprints   = "voiceprints"   // 别的账号本人的声纹（haro-asr 的声纹目录）
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

// voiceprintEraser 删一个账号本人的声纹（经同一个 Pod 里的 haro-asr：它删盘上的、重新加载，立刻不再认）。
// 只删别的账号的本人（accounts.VoiceprintNameOf 写出来的那种）：管理员沿用的声纹名、访客（不登记声纹）不动。
type voiceprintEraser struct {
	asrURL string
	client *http.Client
}

func (e voiceprintEraser) ErasePerson(ctx context.Context, person string) error {
	if _, owner, ok := accounts.ParsePerson(person); !ok || !owner {
		return nil
	}
	address, err := endpoint.Join(e.asrURL, realtimeproto.VoiceprintPath, nil, accounts.VoiceprintNameOf(person))
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, address, nil)
	if err != nil {
		return err
	}
	response, err := e.client.Do(request)
	if err != nil {
		return fmt.Errorf("连 haro-asr 删声纹: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("haro-asr 删声纹回了 %d", response.StatusCode)
	}
	return nil
}
