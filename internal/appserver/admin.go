package appserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/money"
)

// AccountAdmin 是管理员管账号、邀请码（accounts.Store 实现）。
type AccountAdmin interface {
	Approve(ctx context.Context, id string) (accounts.Account, error)
	Disable(ctx context.Context, id string) (accounts.Account, error)
	Enable(ctx context.Context, id string) (accounts.Account, error)
	ScheduleDeletion(ctx context.Context, id string) (accounts.Account, error)
	SetMonthlyCap(ctx context.Context, id string, cap money.Amount) (accounts.Account, error)
	CreateInvite(ctx context.Context, createdBy, note string, ttl time.Duration) (accounts.Invite, string, error)
	Invites(ctx context.Context) ([]accounts.Invite, error)
	RevokeInvite(ctx context.Context, id string) error
}

// accountSummary 是管理员看到的一个账号（运维信息：自己这个月的用量、管理员给它定的上限、终端、版本；看不到内容）。
// 管理员那一行是管理员自己的用量、没有上限（全局的上限在花费页）。
func (s *Server) accountSummary(ctx context.Context, account accounts.Account) (appapi.AccountSummary, error) {
	spent, err := s.opts.Spending.AccountSpent(ctx, billingAccount(account))
	if err != nil {
		return appapi.AccountSummary{}, err
	}
	limit, err := s.opts.Spending.AccountCap(ctx, billingAccount(account))
	if err != nil {
		return appapi.AccountSummary{}, err
	}
	row := appapi.AccountSummary{AccountId: account.ID, DisplayName: account.DisplayName, Role: appapi.Role(account.Role),
		Status: appapi.AccountStatus(account.Status), JoinedVia: appapi.JoinedVia(account.JoinedVia),
		SpentThisMonth: s.moneyTotal(ctx, spent), Cap: optionalMoney(limit)}
	terminals, err := s.terminalCounts(ctx, account)
	if err != nil {
		return appapi.AccountSummary{}, err
	}
	row.TerminalsOnline = terminals.online
	if !account.LastUsedAt.IsZero() {
		lastUsed := account.LastUsedAt
		row.LastUsedAt = &lastUsed
	}
	row.AppVersion = optional(account.AppVersion)
	if !account.DeletionDueAt.IsZero() {
		due := account.DeletionDueAt
		row.DeletionScheduledAt = &due
	}
	return row, nil
}

// manageAccount 是管理员对一个账号做一件事（批准、停用、恢复、删除），回改后的账号；停用、删除的断开它连着的终端。
func (s *Server) manageAccount(ctx context.Context, id string, change func(context.Context, string) (accounts.Account, error)) (
	appapi.AccountSummary, *problemReply) {
	if denied := s.adminOnly(ctx); denied != nil {
		return appapi.AccountSummary{}, denied
	}
	if s.opts.Admin == nil {
		reply := s.notImplemented(ctx)
		return appapi.AccountSummary{}, &reply
	}
	account, err := change(ctx, id)
	var reply problemReply
	switch {
	case errors.Is(err, accounts.ErrNotFound):
		reply = s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil)
	case errors.Is(err, accounts.ErrAccountState), errors.Is(err, accounts.ErrIsAdmin):
		reply = s.fail(ctx, http.StatusConflict, appapi.ErrorCodeConflict, nil)
	case err != nil:
		reply = s.internalError(ctx, "改账号", err)
	default:
		if account.Status == accounts.StatusDisabled {
			s.disconnectTerminals(ctx, account)
		}
		summary, err := s.accountSummary(ctx, account)
		if err != nil {
			reply = s.internalError(ctx, "读账号", err)
			return appapi.AccountSummary{}, &reply
		}
		s.logger.Info("管理员改了账号", "account", account.ID, "status", account.Status, "deletion_due", !account.DeletionDueAt.IsZero(),
			"monthly_cap", account.MonthlyCap.String())
		return summary, nil
	}
	return appapi.AccountSummary{}, &reply
}

// disconnectTerminals 断开这个账号连着的终端（停用以后它们握手会被拒，已经连着的要断开）。
func (s *Server) disconnectTerminals(ctx context.Context, account accounts.Account) {
	if s.opts.Registry == nil || s.opts.Terminals == nil {
		return
	}
	terminals, err := s.opts.Registry.TerminalsOf(ctx, account.ID)
	if err != nil {
		s.logger.Warn("停用账号后找它的终端出错（握手照样会被拒）", "account", account.ID, "err", err)
		return
	}
	for _, terminal := range terminals {
		s.opts.Terminals.Disconnect(terminal.ID)
	}
}

// ApproveAccount 实现 appapi.StrictServerInterface。
func (s *Server) ApproveAccount(ctx context.Context, request appapi.ApproveAccountRequestObject) (appapi.ApproveAccountResponseObject, error) {
	summary, denied := s.manageAccount(ctx, request.AccountId, s.adminApprove)
	if denied != nil {
		return *denied, nil
	}
	return appapi.ApproveAccount200JSONResponse(summary), nil
}

// DisableAccount 实现 appapi.StrictServerInterface。
func (s *Server) DisableAccount(ctx context.Context, request appapi.DisableAccountRequestObject) (appapi.DisableAccountResponseObject, error) {
	summary, denied := s.manageAccount(ctx, request.AccountId, s.adminDisable)
	if denied != nil {
		return *denied, nil
	}
	return appapi.DisableAccount200JSONResponse(summary), nil
}

// EnableAccount 实现 appapi.StrictServerInterface。
func (s *Server) EnableAccount(ctx context.Context, request appapi.EnableAccountRequestObject) (appapi.EnableAccountResponseObject, error) {
	summary, denied := s.manageAccount(ctx, request.AccountId, s.adminEnable)
	if denied != nil {
		return *denied, nil
	}
	return appapi.EnableAccount200JSONResponse(summary), nil
}

// DeleteAccount 实现 appapi.StrictServerInterface。
func (s *Server) DeleteAccount(ctx context.Context, request appapi.DeleteAccountRequestObject) (appapi.DeleteAccountResponseObject, error) {
	summary, denied := s.manageAccount(ctx, request.AccountId, s.adminDelete)
	if denied != nil {
		return *denied, nil
	}
	return appapi.DeleteAccount200JSONResponse(summary), nil
}

// 这几个在 opts.Admin 为 nil 时不会被调用（manageAccount 先回 501）。
func (s *Server) adminApprove(ctx context.Context, id string) (accounts.Account, error) {
	return s.opts.Admin.Approve(ctx, id)
}
func (s *Server) adminDisable(ctx context.Context, id string) (accounts.Account, error) {
	return s.opts.Admin.Disable(ctx, id)
}
func (s *Server) adminEnable(ctx context.Context, id string) (accounts.Account, error) {
	return s.opts.Admin.Enable(ctx, id)
}
func (s *Server) adminDelete(ctx context.Context, id string) (accounts.Account, error) {
	return s.opts.Admin.ScheduleDeletion(ctx, id)
}

// --- 邀请码

func (s *Server) inviteReply(ctx context.Context, invite accounts.Invite) (appapi.Invite, error) {
	reply := appapi.Invite{InviteId: invite.ID, Note: optional(invite.Note), Status: appapi.InviteStatus(invite.Status(time.Now())),
		CreatedAt: invite.CreatedAt, ExpiresAt: invite.ExpiresAt}
	if !invite.UsedAt.IsZero() {
		used := invite.UsedAt
		reply.UsedAt = &used
	}
	if invite.UsedBy != "" {
		account, err := s.accounts.ByID(ctx, invite.UsedBy)
		switch {
		case errors.Is(err, accounts.ErrNotFound):
		case err != nil:
			return appapi.Invite{}, err
		default:
			reply.UsedByName = &account.DisplayName
		}
	}
	return reply, nil
}

// ListInvites 实现 appapi.StrictServerInterface。
func (s *Server) ListInvites(ctx context.Context, _ appapi.ListInvitesRequestObject) (appapi.ListInvitesResponseObject, error) {
	if denied := s.adminOnly(ctx); denied != nil {
		return *denied, nil
	}
	if s.opts.Admin == nil {
		return s.notImplemented(ctx), nil
	}
	invites, err := s.opts.Admin.Invites(ctx)
	if err != nil {
		return s.internalError(ctx, "读邀请码", err), nil
	}
	reply := appapi.ListInvites200JSONResponse{}
	for _, invite := range invites {
		row, err := s.inviteReply(ctx, invite)
		if err != nil {
			return s.internalError(ctx, "读邀请码", err), nil
		}
		reply = append(reply, row)
	}
	return reply, nil
}

// CreateInvite 实现 appapi.StrictServerInterface。
func (s *Server) CreateInvite(ctx context.Context, request appapi.CreateInviteRequestObject) (appapi.CreateInviteResponseObject, error) {
	admin, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if admin.Role != accounts.RoleAdmin {
		return s.fail(ctx, http.StatusForbidden, appapi.ErrorCodeForbidden, nil), nil
	}
	if s.opts.Admin == nil {
		return s.notImplemented(ctx), nil
	}
	ttl := accounts.DefaultInviteTTL
	if request.Body.ExpiresInHours != nil {
		ttl = time.Duration(*request.Body.ExpiresInHours) * time.Hour
	}
	invite, code, err := s.opts.Admin.CreateInvite(ctx, admin.ID, deref(request.Body.Note), ttl)
	if err != nil {
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": err.Error()}), nil
	}
	row, err := s.inviteReply(ctx, invite)
	if err != nil {
		return s.internalError(ctx, "读邀请码", err), nil
	}
	s.logger.Info("管理员生成了邀请码", "invite", invite.ID, "expires", invite.ExpiresAt)
	return appapi.CreateInvite201JSONResponse{Invite: row, Code: code, ShareUrl: s.inviteShareURL(code)}, nil
}

// inviteShareURL 是发给对方的登录地址：浏览器打开就带着这个码开始登录，登录完到网页版；没有网页版、没开登录时为 nil。
func (s *Server) inviteShareURL(code string) *string {
	if s.opts.WebURL == "" || s.opts.Login == nil {
		return nil
	}
	link := strings.TrimRight(s.opts.Login.PublicURL, "/") + startPath + "?" + url.Values{"client": {clientWeb}, "invite": {code}}.Encode()
	return &link
}

// RevokeInvite 实现 appapi.StrictServerInterface。
func (s *Server) RevokeInvite(ctx context.Context, request appapi.RevokeInviteRequestObject) (appapi.RevokeInviteResponseObject, error) {
	if denied := s.adminOnly(ctx); denied != nil {
		return *denied, nil
	}
	if s.opts.Admin == nil {
		return s.notImplemented(ctx), nil
	}
	switch err := s.opts.Admin.RevokeInvite(ctx, request.InviteId); {
	case errors.Is(err, accounts.ErrInviteNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "撤销邀请码", err), nil
	}
	return appapi.RevokeInvite204Response{}, nil
}

// UpdateAccount 实现 appapi.StrictServerInterface：定月上限（默认货币）。
func (s *Server) UpdateAccount(ctx context.Context, request appapi.UpdateAccountRequestObject) (appapi.UpdateAccountResponseObject, error) {
	var limit money.Amount
	if body := request.Body.MonthlyCap; body != nil {
		parsed, err := money.ParseAmount(body.Amount, body.Currency)
		if err != nil || parsed.Currency != s.opts.DefaultCurrency || parsed.Micros <= 0 {
			return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed,
				map[string]string{"detail": "monthly_cap：要大于 0、币种是 " + s.opts.DefaultCurrency}), nil
		}
		limit = parsed
	}
	summary, denied := s.manageAccount(ctx, request.AccountId, func(ctx context.Context, id string) (accounts.Account, error) {
		return s.opts.Admin.SetMonthlyCap(ctx, id, limit)
	})
	if denied != nil {
		return *denied, nil
	}
	return appapi.UpdateAccount200JSONResponse(summary), nil
}
