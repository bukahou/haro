package spending

import (
	"context"

	"github.com/bukahou/haro/internal/brain"
)

// AccountOf 把 ctx 里标的人（brain.WithBillingPerson）换成账号；空 = 服务端主人（管理员）。accounts.BillingAccountOf 实现。
type AccountOf func(person string) string

// WithAttribution 给账本套一层：每一笔按 ctx 里标的人记上账号（已经填了账号的照原样）。套在最外层：写不进、留着补写的那笔也带着账号。
func WithAttribution(ledger Ledger, accountOf AccountOf) Ledger {
	return &attributedLedger{Ledger: ledger, accountOf: accountOf}
}

type attributedLedger struct {
	Ledger
	accountOf AccountOf
}

func (l *attributedLedger) Record(ctx context.Context, charge Charge) error {
	if charge.Account == "" {
		charge.Account = l.accountOf(brain.BillingPersonFrom(ctx))
	}
	return l.Ledger.Record(ctx, charge)
}
