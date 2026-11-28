package accounts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// PersonData 是存着一个人的数据的地方（对话、记忆、提醒、日程、账目、调查……）：彻底删账号时逐个删。
type PersonData interface {
	ErasePerson(ctx context.Context, person string) error
}

// NamedData 是一处数据（名字给日志、测试用）。
type NamedData struct {
	Name string
	Data PersonData
}

// purgeEvery 是多久查一次软删除到期的账号。
const purgeEvery = 24 * time.Hour

// Purger 把软删除到期的账号彻底删掉：先在每一处删掉这个账号的两个人（本人、访客）的数据，全部删成了才删账号
// （账号库里的令牌、资料、家、终端随它删）；有一处出错就留着这个账号，下次再试（删过的再删一次没有副作用）。
//
// 每次之前先把久不连接的匿名账号软删除（IdleAnonymousAfter；它们 DeletionGrace 之后照样彻底删）。
type Purger struct {
	Store  *Store
	Data   []NamedData
	Logger *slog.Logger
	// IdleAnonymousAfter：匿名账号的终端都这么久没连过就软删除；0 = 不清
	IdleAnonymousAfter time.Duration
}

// Run 启动时查一次、之后每天查一次，随 ctx 结束。
func (p Purger) Run(ctx context.Context) {
	ticker := time.NewTicker(purgeEvery)
	defer ticker.Stop()
	for {
		if err := p.PurgeDue(ctx); err != nil {
			p.Logger.Error("彻底删到期的账号出错（明天再试）", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// PurgeDue 彻底删掉现在到期的账号；一个账号出错不影响别的，错误合在一起返回。
func (p Purger) PurgeDue(ctx context.Context) error {
	if err := p.scheduleIdleAnonymous(ctx); err != nil {
		p.Logger.Error("找久不连接的匿名账号出错（照样往下删到期的）", "err", err)
	}
	due, err := p.Store.DueForDeletion(ctx)
	if err != nil {
		return err
	}
	var failed []error
	for _, account := range due {
		if err := p.purge(ctx, account); err != nil {
			failed = append(failed, fmt.Errorf("账号 %s: %w", account.ID, err))
			continue
		}
		p.Logger.Info("软删除到期，彻底删掉了账号和它的数据", "account", account.ID, "places", len(p.Data))
	}
	return errors.Join(failed...)
}

func (p Purger) purge(ctx context.Context, account Account) error {
	for _, place := range p.Data {
		for _, person := range []string{account.OwnerPerson, account.GuestPerson} {
			if err := place.Data.ErasePerson(ctx, person); err != nil {
				return fmt.Errorf("删%s: %w", place.Name, err)
			}
		}
	}
	return p.Store.Purge(ctx, account.ID)
}

// scheduleIdleAnonymous 把久不连接的匿名账号软删除（30 天内主人在 App 里看得到、恢复得了）。
func (p Purger) scheduleIdleAnonymous(ctx context.Context) error {
	if p.IdleAnonymousAfter <= 0 {
		return nil
	}
	idle, err := p.Store.IdleAnonymous(ctx, p.Store.now().Add(-p.IdleAnonymousAfter))
	if err != nil {
		return err
	}
	for _, account := range idle {
		if _, err := p.Store.ScheduleDeletion(ctx, account.ID); err != nil {
			return fmt.Errorf("软删除久不连接的匿名账号 %s: %w", account.ID, err)
		}
		p.Logger.Info("匿名账号久不连接：软删除（到期后彻底删）", "account", account.ID, "idle", p.IdleAnonymousAfter)
	}
	return nil
}
