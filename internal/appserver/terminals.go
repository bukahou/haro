package appserver

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/buildinfo"
)

// 配对码输错的上限：同一个账号 pairFailureWindow 内输错 pairFailureLimit 次后先停一停（来源 IP 的限流之外再加一层：
// 换着地方试也算在账号头上）。
const (
	pairFailureLimit  = 5
	pairFailureWindow = 10 * time.Minute
)

// pairFailures 记着每个账号最近输错配对码的时间（只在内存里；服务端重启就清零，限流照样挡着）。
type pairFailures struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func (p *pairFailures) recent(accountID string, now time.Time) []time.Time {
	var kept []time.Time
	for _, at := range p.failures[accountID] {
		if now.Sub(at) < pairFailureWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(p.failures, accountID)
	} else {
		p.failures[accountID] = kept
	}
	return kept
}

// blocked 判断这个账号现在是不是输错太多次了。
func (p *pairFailures) blocked(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.recent(accountID, time.Now())) >= pairFailureLimit
}

// fail 记下一次输错。
func (p *pairFailures) fail(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.failures[accountID] = append(p.recent(accountID, now), now)
}

// terminalCounts 是一个账号的终端：一共几台、在线几台、几台要升级。管理员账号还算上没带设备凭证、连着的（过渡期都归管理员）。
type terminalCounts struct{ total, online, needsUpgrade int }

func (s *Server) terminalCounts(ctx context.Context, account accounts.Account) (terminalCounts, error) {
	var counts terminalCounts
	if account.Role == accounts.RoleAdmin && s.opts.Terminals != nil {
		counts.online, counts.needsUpgrade = s.opts.Terminals.Unidentified()
		counts.total = counts.online
	}
	if s.opts.Registry == nil {
		return counts, nil
	}
	terminals, err := s.opts.Registry.TerminalsOf(ctx, account.ID)
	if err != nil {
		return terminalCounts{}, err
	}
	for _, terminal := range terminals {
		view := s.terminalOf(ctx, terminal)
		counts.total++
		if view.Online {
			counts.online++
		}
		if view.NeedsUpgrade {
			counts.needsUpgrade++
		}
	}
	return counts, nil
}

// terminalOf 是给 App 看的一台终端：在线时版本取它这次报的，不在线时取上次连进来时报的。
func (s *Server) terminalOf(ctx context.Context, terminal accounts.Terminal) appapi.Terminal {
	view := appapi.Terminal{TerminalId: terminal.ID, Name: terminal.Name, Version: terminal.Version, LastSeenAt: terminal.LastSeenAt,
		Network: networkOf(terminal.LastRemote)}
	if s.opts.Terminals != nil {
		if online, version := s.opts.Terminals.Status(terminal.ID); online {
			view.Online, view.Version = true, version
		}
	}
	if view.Version != "" && buildinfo.Mismatch(buildinfo.Version, view.Version) {
		view.NeedsUpgrade = true
		note := s.texts.say(languageFrom(ctx), "terminal.upgrade_note", nil)
		view.UpgradeNote = &note
	}
	return view
}

// networkOf 是从哪里连来的：私有地址、本机算局域网，其余算外网；还没连过时算局域网（现在只有局域网能登记）。
func networkOf(remote string) appapi.TerminalNetwork {
	address, err := netip.ParseAddr(remote)
	if err != nil || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return appapi.Lan
	}
	return appapi.Internet
}

// ListTerminals 实现 appapi.StrictServerInterface。
func (s *Server) ListTerminals(ctx context.Context, _ appapi.ListTerminalsRequestObject) (appapi.ListTerminalsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	terminals, err := s.opts.Registry.TerminalsOf(ctx, account.ID)
	if err != nil {
		return s.internalError(ctx, "读终端", err), nil
	}
	reply := appapi.ListTerminals200JSONResponse{}
	for _, terminal := range terminals {
		reply = append(reply, s.terminalOf(ctx, terminal))
	}
	return reply, nil
}

// PairTerminal 实现 appapi.StrictServerInterface：输入终端显示的配对码，把它绑到这个账号。
func (s *Server) PairTerminal(ctx context.Context, request appapi.PairTerminalRequestObject) (appapi.PairTerminalResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	if s.pairing.blocked(account.ID) {
		return s.fail(ctx, http.StatusTooManyRequests, appapi.ErrorCodePairingAttemptsExceeded, nil), nil
	}
	terminal, err := s.opts.Registry.PairRegistration(ctx, account.ID, strings.TrimSpace(request.Body.Code))
	switch {
	case errors.Is(err, accounts.ErrPairCodeInvalid):
		s.pairing.fail(account.ID)
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodePairingCodeInvalid, nil), nil
	case errors.Is(err, accounts.ErrPairCodeExpired):
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodePairingCodeExpired, nil), nil
	case errors.Is(err, accounts.ErrPairCodeUsed):
		return s.fail(ctx, http.StatusConflict, appapi.ErrorCodeConflict, nil), nil
	case err != nil:
		return s.internalError(ctx, "配对终端", err), nil
	}
	s.logger.Info("终端配对了", "account", account.ID, "terminal", terminal.ID, "name", terminal.Name)
	// 匿名期间的数据并进账号在第 21 段做（别人的匿名终端才有）：现在配对的都是刚登记的终端，没有要并的
	return appapi.PairTerminal200JSONResponse{Terminal: s.terminalOf(ctx, terminal)}, nil
}

// RenameTerminal 实现 appapi.StrictServerInterface。
func (s *Server) RenameTerminal(ctx context.Context, request appapi.RenameTerminalRequestObject) (appapi.RenameTerminalResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	name := strings.TrimSpace(request.Body.Name)
	if name == "" {
		return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": "name"}), nil
	}
	terminal, err := s.opts.Registry.RenameTerminal(ctx, account.ID, request.TerminalId, name)
	switch {
	case errors.Is(err, accounts.ErrTerminalNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "改终端的名字", err), nil
	}
	return appapi.RenameTerminal200JSONResponse(s.terminalOf(ctx, terminal)), nil
}

// RemoveTerminal 实现 appapi.StrictServerInterface：凭证作废、正连着的会话马上断开。
func (s *Server) RemoveTerminal(ctx context.Context, request appapi.RemoveTerminalRequestObject) (appapi.RemoveTerminalResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	err := s.opts.Registry.RemoveTerminal(ctx, account.ID, request.TerminalId)
	switch {
	case errors.Is(err, accounts.ErrTerminalNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "移除终端", err), nil
	}
	if s.opts.Terminals != nil {
		s.opts.Terminals.Disconnect(request.TerminalId)
	}
	s.logger.Info("终端移除了", "account", account.ID, "terminal", request.TerminalId)
	return appapi.RemoveTerminal204Response{}, nil
}

// ListPendingTerminals 实现 appapi.StrictServerInterface：等着配对 / 批准的新终端（只有管理员看得到）。
func (s *Server) ListPendingTerminals(ctx context.Context, _ appapi.ListPendingTerminalsRequestObject) (appapi.ListPendingTerminalsResponseObject, error) {
	if denied := s.adminOnly(ctx); denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	pending, err := s.opts.Registry.PendingRegistrations(ctx)
	if err != nil {
		return s.internalError(ctx, "读新终端", err), nil
	}
	reply := appapi.ListPendingTerminals200JSONResponse{}
	for _, registration := range pending {
		reply = append(reply, appapi.PendingTerminal{TerminalId: registration.ID, ReportedName: registration.Name,
			ShortId: registration.ShortID, RequestedAt: registration.CreatedAt, Network: networkOf(registration.Remote),
			Version: registration.Version})
	}
	return reply, nil
}

// ApproveTerminal 实现 appapi.StrictServerInterface：批准 = 归一个新的匿名账号。
func (s *Server) ApproveTerminal(ctx context.Context, request appapi.ApproveTerminalRequestObject) (appapi.ApproveTerminalResponseObject, error) {
	if denied := s.adminOnly(ctx); denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	terminal, err := s.opts.Registry.ApproveRegistration(ctx, request.TerminalId)
	switch {
	case errors.Is(err, accounts.ErrRegistrationNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "批准终端", err), nil
	}
	s.logger.Info("管理员批准了新终端（匿名账号）", "terminal", terminal.ID, "account", terminal.AccountID)
	return appapi.ApproveTerminal204Response{}, nil
}

// RejectTerminal 实现 appapi.StrictServerInterface。
func (s *Server) RejectTerminal(ctx context.Context, request appapi.RejectTerminalRequestObject) (appapi.RejectTerminalResponseObject, error) {
	if denied := s.adminOnly(ctx); denied != nil {
		return *denied, nil
	}
	if s.opts.Registry == nil {
		return s.notImplemented(ctx), nil
	}
	err := s.opts.Registry.RejectRegistration(ctx, request.TerminalId)
	switch {
	case errors.Is(err, accounts.ErrRegistrationNotFound):
		return s.fail(ctx, http.StatusNotFound, appapi.ErrorCodeNotFound, nil), nil
	case err != nil:
		return s.internalError(ctx, "拒绝终端", err), nil
	}
	return appapi.RejectTerminal204Response{}, nil
}

// adminOnly 不是管理员时回 403（没登录时回 401）。
func (s *Server) adminOnly(ctx context.Context) *problemReply {
	account, denied := s.me(ctx)
	if denied != nil {
		return denied
	}
	if account.Role != accounts.RoleAdmin {
		reply := s.fail(ctx, http.StatusForbidden, appapi.ErrorCodeForbidden, nil)
		return &reply
	}
	return nil
}
