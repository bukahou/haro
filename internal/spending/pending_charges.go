package spending

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/sqliteschema"

	"github.com/bukahou/haro/internal/money"
)

// maxPendingCharges 是账本写不进时内存里最多留几笔待补写。写不进之后，有上限的付费调用、搜索都不放行（Guard），
// 这期间新增的只有已经在进行的那几次和不受额度管的小调用，几百笔足够；满了以后再来的记不上（打日志、报错）。
const maxPendingCharges = 500

// closeFlushTimeout 是关停时最后补写一次最多等多久（和 SQLite 等锁的上限一样长）。
const closeFlushTimeout = sqliteschema.BusyTimeout

// pendingChargesLedger 给账本套一层：写不进的那笔留在内存里（最多 maxPendingCharges 笔），之后补写。
// 补写的时机：下一次记账之前、额度检查之前（FlushPending）、就绪检查（CheckWritable）、关停（Close）。
// 补写不成时 FlushPending 报错，Guard 据此拒绝要花钱的调用：写不进时合计不再增长，上限就管不住花费了。
type pendingChargesLedger struct {
	Ledger // 里面那层（记上一笔打一行日志的账本）
	logger *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	pending []Charge // 按发生的先后
}

// Record 先补写之前没写进的，再记这一笔；写不进时留在内存里，返回说明（这一笔先留着，或者待补写的满了没记上）。
func (l *pendingChargesLedger) Record(ctx context.Context, charge Charge) error {
	if charge.At.IsZero() { // 补写时按发生的时间记，算进它发生的那一天、那个月
		charge.At = l.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.flushLocked(ctx); err != nil {
		return l.keepLocked(charge, err)
	}
	if err := l.Ledger.Record(ctx, charge); err != nil {
		return l.keepLocked(charge, err)
	}
	return nil
}

// FlushPending 把之前没写进的补写进去；补写不成时报错（这时账本的合计不完整）。
func (l *pendingChargesLedger) FlushPending(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.flushLocked(ctx)
}

// CheckWritable 先补写之前没写进的（补写不成就是写不进），再确认拿得到写锁。
func (l *pendingChargesLedger) CheckWritable(ctx context.Context) error {
	if err := l.FlushPending(ctx); err != nil {
		return err
	}
	return l.Ledger.CheckWritable(ctx)
}

// Close 关停前最后补写一次；还写不进的就丢了，打日志说明丢了几笔、多少钱。
func (l *pendingChargesLedger) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeFlushTimeout)
	defer cancel()
	l.mu.Lock()
	if err := l.flushLocked(ctx); err != nil {
		lost := money.Sum{}
		for _, charge := range l.pending {
			lost.Add(charge.Cost)
		}
		l.logger.Error("💴 关停时还有花费没写进账本：这几笔丢了", "charges", len(l.pending), "cost", lost.String(), "err", err)
		l.pending = nil
	}
	l.mu.Unlock()
	return l.Ledger.Close()
}

// flushLocked 按先后补写；写进一笔去掉一笔，遇到写不进的就停下报错。调用方持有 l.mu。
func (l *pendingChargesLedger) flushLocked(ctx context.Context) error {
	written := 0
	for len(l.pending) > 0 {
		if err := l.Ledger.Record(ctx, l.pending[0]); err != nil {
			if written > 0 {
				l.logger.Warn("💴 之前没写进账本的补写了一部分", "written", written, "pending", len(l.pending), "err", err)
			}
			return fmt.Errorf("还有 %d 笔花费没写进账本: %w", len(l.pending), err)
		}
		l.pending = l.pending[1:]
		written++
	}
	if written > 0 {
		l.pending = nil
		l.logger.Warn("💴 账本恢复：之前没写进的都补写了，额度检查照常放行", "written", written)
	}
	return nil
}

// keepLocked 把写不进的这一笔留在内存里；满了就不留（这一笔记不上）。返回给调用方的说明带着原因。调用方持有 l.mu。
func (l *pendingChargesLedger) keepLocked(charge Charge, cause error) error {
	if len(l.pending) >= maxPendingCharges {
		l.logger.Error("💴 账本写不进，内存里待补写的也满了：这一笔记不上", "pending", len(l.pending),
			"purpose", charge.Purpose, "item", charge.Item, "cost", charge.Cost.String(), "credits", charge.Credits, "err", cause)
		return fmt.Errorf("账本写不进，待补写的已满 %d 笔，这一笔没记上: %w", maxPendingCharges, cause)
	}
	if len(l.pending) == 0 {
		l.logger.Error("💴 账本写不进：写不进的先留在内存里、之后补写；补写成功之前，有上限的付费调用和搜索都不放行", "err", cause)
	}
	l.pending = append(l.pending, charge)
	return fmt.Errorf("账本写不进，这一笔先留在内存里、之后补写（待补写 %d 笔）: %w", len(l.pending), cause)
}
