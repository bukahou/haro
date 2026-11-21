package spending

import (
	"context"
	"log/slog"
)

// loggingLedger 给账本套一层：每记一笔都打一行日志。花的钱在日志里就看得见（包括被打断、出错的那几笔），
// 核对时不用进账本查。没记上的不打，由调用方说明。
type loggingLedger struct {
	Ledger
	logger *slog.Logger
}

// WithChargeLog 返回每记一笔都打日志的账本。
func WithChargeLog(ledger Ledger, logger *slog.Logger) Ledger {
	return &loggingLedger{Ledger: ledger, logger: logger}
}

// Record 记一笔，记上了就打一行：用途、花在什么上、日元，加上 token 或积分；不知道的数不写（不编成 0）。
func (l *loggingLedger) Record(ctx context.Context, charge Charge) error {
	if err := l.Ledger.Record(ctx, charge); err != nil {
		return err
	}
	args := []any{"purpose", charge.Purpose, "item", charge.Item, "yen", charge.Yen}
	switch {
	case charge.Credits > 0:
		args = append(args, "credits", charge.Credits)
	case charge.TokensUnknown:
		args = append(args, "tokens_unknown", true)
	default:
		args = append(args, "input_tokens", charge.InputTokens, "cache_read_tokens", charge.CacheReadTokens,
			"cache_write_tokens", charge.CacheWriteTokens)
		if !charge.Incomplete { // 被打断、出错时输出数不知道
			args = append(args, "output_tokens", charge.OutputTokens)
		}
	}
	if charge.Incomplete {
		l.logger.Info("记账：这次请求没正常结束，只记知道的部分", append(args, "incomplete", true)...)
		return nil
	}
	l.logger.Info("记账", args...)
	return nil
}

// RaiseCap 记下提高上限，记上了就打一行（谁、多少）。
func (l *loggingLedger) RaiseCap(ctx context.Context, raise CapRaise) error {
	if err := l.Ledger.RaiseCap(ctx, raise); err != nil {
		return err
	}
	l.logger.Info("记账：主人确认临时提高这个月的上限", "yen", raise.Yen, "person", raise.Person)
	return nil
}
