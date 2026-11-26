package memory

import (
	"context"
	"time"

	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/notes"
)

// Run 在后台把向量补齐，直到 ctx 结束：先把库里已有的向量载入索引，之后每隔 indexInterval（或记住新的一条时马上）
// 把没算过向量的记事和对话轮次算上、存下、放进索引。没配嵌入服务时什么都不做。
// 嵌入服务连不上时等下一次再试（开始失败、恢复时各记一条日志）。
func (k *Keeper) Run(ctx context.Context) {
	if k.embedder == nil {
		return
	}
	loaded := false
	for {
		if !loaded {
			loaded = k.load(ctx)
		}
		if loaded {
			k.indexPending(ctx)
		}
		indexEntries.Record(ctx, float64(k.index.Len()))
		timer := time.NewTimer(indexInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-k.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// load 把库里这个模型的向量载入索引；读不出来时记警告、返回 false（下一次再载）。
func (k *Keeper) load(ctx context.Context) bool {
	model := k.embedder.Model()
	noteVectors, err := k.notes.Vectors(ctx, model)
	if err != nil {
		k.logger.Warn("载入记事的向量失败：过一会儿再试", "err", err)
		return false
	}
	for _, item := range noteVectors {
		k.index.Put(KindNote, item.Person, item.ID, item.Vector)
	}
	turns := 0
	if k.transcripts != nil {
		turnVectors, err := k.transcripts.TurnVectors(ctx, model)
		if err != nil {
			k.logger.Warn("载入对话的向量失败：过一会儿再试", "err", err)
			return false
		}
		for _, item := range turnVectors {
			k.index.Put(KindTurn, item.Person, item.Seq, item.Vector)
		}
		turns = len(turnVectors)
	}
	k.logger.Info("长期记忆的向量已载入", "model", model, "notes", len(noteVectors), "turns", turns)
	return true
}

// indexPending 算还没有向量的记事和对话轮次（每一轮最多 indexBatchesPerRound 批）。
func (k *Keeper) indexPending(ctx context.Context) {
	model := k.embedder.Model()
	for range indexBatchesPerRound {
		pending, err := k.notes.Unindexed(ctx, model, indexBatch)
		if err != nil {
			k.logger.Warn("读没算过向量的记事失败", "err", err)
			return
		}
		if len(pending) == 0 {
			break
		}
		if err := k.indexNotes(ctx, pending); err != nil {
			k.embedFailed(err)
			return
		}
	}
	if k.transcripts == nil {
		return
	}
	for range indexBatchesPerRound {
		pending, err := k.transcripts.UnindexedTurns(ctx, model, indexBatch)
		if err != nil {
			k.logger.Warn("读没算过向量的对话失败", "err", err)
			return
		}
		if len(pending) == 0 {
			return
		}
		if err := k.indexTurns(ctx, pending); err != nil {
			k.embedFailed(err)
			return
		}
	}
}

// indexNotes 算这几条记事的向量、存下、放进索引。
func (k *Keeper) indexNotes(ctx context.Context, list []notes.Note) error {
	texts := make([]string, len(list))
	for i, note := range list {
		texts[i] = note.Content
	}
	vectors, err := k.embedDocuments(ctx, texts)
	if err != nil {
		return err
	}
	model := k.embedder.Model()
	for i, note := range list {
		if err := k.notes.SaveVector(ctx, note.ID, model, vectors[i]); err != nil {
			return err
		}
		k.index.Put(KindNote, note.Person, note.ID, vectors[i])
	}
	return nil
}

// indexTurns 算这几轮对话的向量、存下、放进索引。没有文字的轮次（例如什么都没说就被打断）存一个零向量占位：
// 不进索引，也不会每次都被挑出来。
func (k *Keeper) indexTurns(ctx context.Context, turns []conversationstore.TranscriptTurn) error {
	var texts []string
	var withText []conversationstore.TranscriptTurn
	for _, turn := range turns {
		if text := turn.Text(); text != "" {
			texts = append(texts, text)
			withText = append(withText, turn)
		}
	}
	vectors, err := k.embedDocuments(ctx, texts)
	if err != nil {
		return err
	}
	model := k.embedder.Model()
	for _, turn := range turns {
		if turn.Text() == "" {
			if err := k.transcripts.SaveTurnVector(ctx, turn.Person, turn.Seq, model, turn.Digest(), []float32{0}); err != nil {
				return err
			}
		}
	}
	for i, turn := range withText {
		if err := k.transcripts.SaveTurnVector(ctx, turn.Person, turn.Seq, model, turn.Digest(), vectors[i]); err != nil {
			return err
		}
		k.index.Put(KindTurn, turn.Person, turn.Seq, vectors[i])
	}
	return nil
}

func (k *Keeper) embedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	started := time.Now()
	vectors, err := k.embedder.EmbedDocuments(ctx, texts)
	embedDuration.Record(ctx, time.Since(started).Seconds())
	if err == nil && k.failing.CompareAndSwap(true, false) {
		k.logger.Info("嵌入服务恢复了：接着补向量")
	}
	return vectors, err
}

// embedFailed：嵌入服务连不上（第一次记警告，之后不刷屏），下一次再试。
func (k *Keeper) embedFailed(err error) {
	if k.failing.CompareAndSwap(false, true) {
		k.logger.Warn("嵌入服务连不上：记忆照样存，向量之后补", "err", err)
	}
}
