package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// terminalAuthenticator 把账号库接到连接层：验终端握手带的设备凭证，记下它这次连进来的时间、版本、地址。
type terminalAuthenticator struct {
	store  *accounts.Store
	logger *slog.Logger
}

// Authenticate 实现 brainserver.Authenticator。账号停用、还在等同意时也当成凭证无效（终端提示重新配对）。
func (a terminalAuthenticator) Authenticate(ctx context.Context, credential, version, remote string) (brainserver.Identity, error) {
	terminal, account, err := a.store.AuthenticateTerminal(ctx, credential)
	switch {
	case errors.Is(err, accounts.ErrInvalidCredential), errors.Is(err, accounts.ErrDisabled), errors.Is(err, accounts.ErrPendingApproval):
		return brainserver.Identity{}, errors.Join(brainserver.ErrInvalidCredential, err)
	case err != nil:
		return brainserver.Identity{}, err
	}
	if err := a.store.TouchTerminal(ctx, terminal.ID, version, remote); err != nil {
		a.logger.Warn("记终端最近一次连进来失败（不影响连接）", "terminal", terminal.ID, "err", err)
	}
	return brainserver.Identity{TerminalID: terminal.ID, AccountID: account.ID, Owner: account.OwnerPerson, Guest: account.GuestPerson,
		Admin: account.Role == accounts.RoleAdmin}, nil
}

// terminalRegistrar 把账号库接到终端登记的接口（照 RFC 8628）。
type terminalRegistrar struct{ store *accounts.Store }

// StartRegistration 实现 brainserver.Registrar。
func (r terminalRegistrar) StartRegistration(ctx context.Context, name, version, remote string) (realtimeproto.RegistrationStarted, error) {
	registration, err := r.store.StartRegistration(ctx, name, version, remote)
	if err != nil {
		return realtimeproto.RegistrationStarted{}, err
	}
	return realtimeproto.RegistrationStarted{Code: registration.Code, ShortID: registration.ShortID, PollToken: registration.PollToken,
		ExpiresIn: int(accounts.RegistrationTTL.Seconds()), Interval: int(accounts.RegistrationPollInterval.Seconds())}, nil
}

// PollRegistration 实现 brainserver.Registrar。
func (r terminalRegistrar) PollRegistration(ctx context.Context, pollToken string) (realtimeproto.RegistrationOutcome, error) {
	result, err := r.store.PollRegistration(ctx, pollToken)
	if errors.Is(err, accounts.ErrRegistrationNotFound) {
		return realtimeproto.RegistrationOutcome{}, brainserver.ErrRegistrationNotFound
	} else if err != nil {
		return realtimeproto.RegistrationOutcome{}, err
	}
	outcome := realtimeproto.RegistrationOutcome{TerminalID: result.TerminalID, Credential: result.Credential}
	switch result.Status {
	case accounts.RegistrationApproved:
		outcome.Status = realtimeproto.RegistrationApproved
	case accounts.RegistrationRejected:
		outcome.Status = realtimeproto.RegistrationRejected
	case accounts.RegistrationExpired:
		outcome.Status = realtimeproto.RegistrationExpired
	default:
		outcome.Status = realtimeproto.RegistrationPending
	}
	return outcome, nil
}

// residentsOf 是这条连接的终端所属账号的两个人：管理员账号的终端、没带设备凭证的终端不分账号（声纹名照用）。
func residentsOf(terminal brainserver.Terminal) brain.Residents {
	if terminal.Identity == nil || terminal.Identity.Admin {
		return brain.Residents{}
	}
	return brain.Residents{Owner: terminal.Identity.Owner, Guest: terminal.Identity.Guest}
}
