package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/notes"
)

// startMemory 装好长期记忆（v10）并起后台补向量：记事库（记事已并进记忆）、嵌入服务（没配时为 nil：不按意思找）、
// 对话库的完整记录（没配时以前的对话查不了）。返回的 stop 等后台补向量的循环退出（之后才关记事库、对话库）。
func startMemory(ctx context.Context, settings config.ServerSettings, store notes.Repository, deps services,
	logger *slog.Logger) (*memory.Keeper, func() error) {
	var embedder memory.Embedder // 没配嵌入服务时是 nil 接口
	if deps.embedder != nil {
		embedder = deps.embedder
	}
	var transcripts memory.Transcripts
	if deps.conversations != nil {
		transcripts = deps.conversations
	}
	keeper := memory.NewKeeper(store, embedder, transcripts, settings.NotesListLimit, logger)
	running := make(chan struct{})
	go func() {
		defer close(running)
		keeper.Run(ctx) // ctx 随退出信号结束
	}()
	logger.Info("长期记忆已开启", "by_meaning", keeper.SearchesByMeaning(), "past_conversations", transcripts != nil && embedder != nil)
	return keeper, func() error {
		select {
		case <-running:
		case <-time.After(shutdownMemory):
			logger.Warn("长期记忆的后台没在期限内停下", "timeout", shutdownMemory)
		}
		return nil
	}
}
