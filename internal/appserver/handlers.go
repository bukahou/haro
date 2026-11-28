package appserver

import (
	"context"
	"net/http"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appapi"
	"github.com/bukahou/haro/internal/money"
	"github.com/bukahou/haro/internal/spending"
)

// pageSize 是列表每页多少条（client-config 给 App）。
const pageSize = 20

// fail 是出错的回答（说明按这次请求的语言）。
func (s *Server) fail(ctx context.Context, status int, code appapi.ErrorCode, values map[string]string) problemReply {
	language := languageFrom(ctx)
	if language == "" {
		language = s.texts.Default
	}
	detail := s.texts.say(language, "problem."+string(code), values)
	return problemReply{problem: appapi.Problem{Type: problemType, Title: http.StatusText(status), Status: status, Code: code, Detail: &detail}}
}

// notImplemented 是契约里有、这一步还没做的接口（分批上线）：如实回 501，⛔ 不装成成功。
func (s *Server) notImplemented(ctx context.Context) problemReply {
	return s.fail(ctx, http.StatusNotImplemented, appapi.ErrorCodeNotImplemented, nil)
}

// internalError 记下出错的原因，回 500（原因不给 App 看）。
func (s *Server) internalError(ctx context.Context, what string, err error) problemReply {
	s.logger.Error("App 接口出错", "what", what, "err", err)
	return s.fail(ctx, http.StatusInternalServerError, appapi.ErrorCodeInternal, nil)
}

// me 是这次请求验过的账号；拿不到时（不该发生）回 401。
func (s *Server) me(ctx context.Context) (accounts.Account, *problemReply) {
	account, ok := callerFrom(ctx)
	if !ok {
		reply := s.fail(ctx, http.StatusUnauthorized, appapi.ErrorCodeUnauthorized, nil)
		return accounts.Account{}, &reply
	}
	return account, nil
}

// currencyOf 是账号的默认货币（账号没改时用服务端的）。
func (s *Server) currencyOf(account accounts.Account) string {
	if account.DefaultCurrency != "" {
		return account.DefaultCurrency
	}
	return s.opts.DefaultCurrency
}

func moneyOf(amount money.Amount) appapi.Money {
	return appapi.Money{Amount: amount.Rounded(), Currency: amount.Currency}
}

// optionalMoney 是「零值 = 不设」的金额（上限这类）：不设时为 nil。
func optionalMoney(amount money.Amount) *appapi.Money {
	if amount.Micros <= 0 {
		return nil
	}
	value := moneyOf(amount)
	return &value
}

// moneyTotal 是一笔花费的合计。
func (s *Server) moneyTotal(ctx context.Context, spent spending.Spent) appapi.MoneyTotal {
	return s.convertedTotal(ctx, spent.ByCurrency, spent.Total)
}

// convertedTotal 是一笔合计（换成默认货币 + 按币种的原始合计 + 有换算、有没算进去的币种时的提示）。
func (s *Server) convertedTotal(ctx context.Context, byCurrency money.Sum, converted money.Total) appapi.MoneyTotal {
	total := appapi.MoneyTotal{Total: moneyOf(converted.Amount), ByCurrency: []appapi.Money{}}
	for _, code := range byCurrency.Currencies() {
		total.ByCurrency = append(total.ByCurrency, moneyOf(byCurrency.Amount(code)))
	}
	var notes []string
	if len(converted.Converted) > 0 {
		notes = append(notes, s.texts.say(languageFrom(ctx), "conversion", map[string]string{"currencies": strings.Join(converted.Converted, "、"),
			"date": converted.RateDate.Format(time.DateOnly), "target": converted.Amount.Currency}))
		date := openapi_types.Date{Time: converted.RateDate}
		total.RateDate = &date
	}
	if len(converted.Missing) > 0 {
		notes = append(notes, s.texts.say(languageFrom(ctx), "conversion_missing", map[string]string{"currencies": strings.Join(converted.Missing, "、")}))
	}
	if len(notes) > 0 {
		note := strings.Join(notes, " ")
		total.ConversionNote = &note
	}
	return total
}

// monthSpending 是这个月的花费（概览、花费页共用）。
func (s *Server) monthSpending(ctx context.Context, summary spending.Summary) appapi.MonthSpending {
	month := appapi.MonthSpending{Spent: s.moneyTotal(ctx, summary.Month), Status: appapi.SpendingStatus(summary.Status),
		CanRaise: summary.CanRaise, Cap: optionalMoney(summary.MonthCap), RaiseAmount: optionalMoney(summary.Raise)}
	if summary.MonthCap.Micros > 0 {
		left := money.Amount{Micros: max(summary.MonthCap.Micros-summary.Month.Total.Amount.Micros, 0), Currency: summary.Currency}
		value := moneyOf(left)
		ratio := min(float64(summary.Month.Total.Amount.Micros)/float64(summary.MonthCap.Micros), 1)
		month.Remaining, month.UsedRatio = &value, &ratio
	}
	return month
}

// GetClientConfig 实现 appapi.StrictServerInterface。字数上限从契约里取（只写一处）。
func (s *Server) GetClientConfig(ctx context.Context, _ appapi.GetClientConfigRequestObject) (appapi.GetClientConfigResponseObject, error) {
	config := appapi.ClientConfig{ServerVersion: serverVersion(), ApiVersion: s.spec.Info.Version, MinAppVersion: s.opts.MinAppVersion,
		Limits: appapi.Limits{ProfileMaxChars: s.opts.ProfileMaxChars, PageSize: pageSize, AttachmentMaxBytes: attachmentMaxBytes,
			CalendarSyncDaysBack: calendarSyncDaysBack, CalendarSyncDaysAhead: calendarSyncDaysAhead,
			DisplayNameMaxChars: s.maxLength("MeUpdate", "display_name"), TerminalNameMaxChars: s.maxLength("TerminalUpdate", "name")}}
	if len(s.opts.CapOptions) > 0 {
		options := make([]appapi.Money, 0, len(s.opts.CapOptions))
		for _, value := range s.opts.CapOptions {
			options = append(options, moneyOf(money.Of(value, s.opts.DefaultCurrency)))
		}
		config.MonthlyCapOptions = &options
	}
	return appapi.GetClientConfig200JSONResponse(config), nil
}

// maxLength 是契约里某个字段的 maxLength（没写时为 0）。
func (s *Server) maxLength(schema, property string) int {
	ref := s.spec.Components.Schemas[schema]
	if ref == nil || ref.Value == nil || ref.Value.Properties[property] == nil || ref.Value.Properties[property].Value == nil {
		return 0
	}
	if limit := ref.Value.Properties[property].Value.MaxLength; limit != nil {
		return int(*limit)
	}
	return 0
}

func (s *Server) meOf(account accounts.Account) appapi.Me {
	return appapi.Me{AccountId: account.ID, CreatedAt: account.CreatedAt, DefaultCurrency: s.currencyOf(account),
		DisplayName: account.DisplayName, Role: appapi.Role(account.Role), Status: appapi.AccountStatus(account.Status)}
}

// GetMe 实现 appapi.StrictServerInterface。
func (s *Server) GetMe(ctx context.Context, _ appapi.GetMeRequestObject) (appapi.GetMeResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	return appapi.GetMe200JSONResponse(s.meOf(account)), nil
}

// UpdateMe 实现 appapi.StrictServerInterface。
func (s *Server) UpdateMe(ctx context.Context, request appapi.UpdateMeRequestObject) (appapi.UpdateMeResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	var name, currency string
	if request.Body != nil && request.Body.DisplayName != nil {
		name = strings.TrimSpace(*request.Body.DisplayName)
	}
	if request.Body != nil && request.Body.DefaultCurrency != nil {
		code, err := money.ParseCurrency(*request.Body.DefaultCurrency)
		if err != nil {
			return s.fail(ctx, http.StatusBadRequest, appapi.ErrorCodeValidationFailed, map[string]string{"detail": err.Error()}), nil
		}
		currency = code
	}
	updated, err := s.accounts.UpdateProfile(ctx, account.ID, name, currency)
	if err != nil {
		return s.internalError(ctx, "改账号", err), nil
	}
	return appapi.UpdateMe200JSONResponse(s.meOf(updated)), nil
}

// GetOverview 实现 appapi.StrictServerInterface。
func (s *Server) GetOverview(ctx context.Context, _ appapi.GetOverviewRequestObject) (appapi.GetOverviewResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	var overview appapi.Overview
	overview.Account.DisplayName, overview.Account.Role = account.DisplayName, appapi.Role(account.Role)
	terminals, err := s.terminalCounts(ctx, account)
	if err != nil {
		return s.internalError(ctx, "数终端", err), nil
	}
	overview.Terminals.Total, overview.Terminals.Online, overview.Terminals.NeedsUpgrade = terminals.total, terminals.online, terminals.needsUpgrade
	if s.opts.Memory != nil {
		resident, notes, err := s.opts.Memory.Counts(ctx, account.OwnerPerson)
		if err != nil {
			return s.internalError(ctx, "数记忆", err), nil
		}
		overview.Memory.Resident, overview.Memory.Notes = resident, notes
	}
	if s.opts.Research != nil {
		running, err := s.opts.Research.Running(ctx, account.OwnerPerson)
		if err != nil {
			return s.internalError(ctx, "数调查", err), nil
		}
		overview.Research.Running = running
	}
	summary, err := s.spendingFor(ctx, account)
	if err != nil {
		return s.internalError(ctx, "花费", err), nil
	}
	overview.Spending = s.monthSpending(ctx, summary)
	if account.Role == accounts.RoleAdmin {
		pending, err := s.pendingAccounts(ctx)
		if err != nil {
			return s.internalError(ctx, "数账号", err), nil
		}
		overview.Admin = &struct {
			PendingAccounts  int `json:"pending_accounts"`
			PendingTerminals int `json:"pending_terminals"`
		}{PendingAccounts: pending}
	}
	return appapi.GetOverview200JSONResponse(overview), nil
}

// spendingFor 是这个账号的花费。账本还不分账号（第 20 段才分）：现在只有管理员在用，管理员 = 全部花费；
// 别的账号（还在等同意）什么都用不了，是 0。
func (s *Server) spendingFor(ctx context.Context, account accounts.Account) (spending.Summary, error) {
	if account.Role == accounts.RoleAdmin {
		return s.opts.Spending.Summary(ctx)
	}
	empty := spending.Spent{ByCurrency: money.Sum{}, Total: money.Total{Amount: money.Amount{Currency: s.currencyOf(account)}}}
	return spending.Summary{Currency: s.currencyOf(account), Month: empty, Status: spending.StatusOK, TodayResearch: empty}, nil
}

func (s *Server) pendingAccounts(ctx context.Context) (int, error) {
	list, err := s.accounts.List(ctx)
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, account := range list {
		if account.Status == accounts.StatusPendingApproval {
			pending++
		}
	}
	return pending, nil
}

// GetSpending 实现 appapi.StrictServerInterface。
func (s *Server) GetSpending(ctx context.Context, _ appapi.GetSpendingRequestObject) (appapi.GetSpendingResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	summary, err := s.spendingFor(ctx, account)
	if err != nil {
		return s.internalError(ctx, "花费", err), nil
	}
	reply := appapi.Spending{Month: s.monthSpending(ctx, summary), ByPurpose: []appapi.PurposeSpending{}}
	monthTotal := summary.Month.Total.Amount.Micros
	for _, purpose := range summary.ByPurpose {
		share := 0.0
		if monthTotal > 0 {
			share = float64(purpose.Spent.Total.Amount.Micros) / float64(monthTotal)
		}
		reply.ByPurpose = append(reply.ByPurpose, appapi.PurposeSpending{Purpose: string(purpose.Purpose),
			Label: s.texts.say(languageFrom(ctx), "purpose."+string(purpose.Purpose), nil), Spent: s.moneyTotal(ctx, purpose.Spent),
			Share: share})
	}
	reply.Today.Research.Spent = s.moneyTotal(ctx, summary.TodayResearch)
	reply.Today.Research.Cap = optionalMoney(summary.ResearchCap)
	if summary.ResearchCap.Micros > 0 {
		left := moneyOf(money.Amount{Micros: max(summary.ResearchCap.Micros-summary.TodayResearch.Total.Amount.Micros, 0),
			Currency: summary.Currency})
		reply.Today.Research.Remaining = &left
	}
	reply.Today.Search.Used = summary.SearchCredits
	if summary.SearchCap > 0 {
		reply.Today.Search.Cap = &summary.SearchCap
	}
	return appapi.GetSpending200JSONResponse(reply), nil
}

// ListAccounts 实现 appapi.StrictServerInterface：只有管理员能看，只给运维信息。
func (s *Server) ListAccounts(ctx context.Context, _ appapi.ListAccountsRequestObject) (appapi.ListAccountsResponseObject, error) {
	account, denied := s.me(ctx)
	if denied != nil {
		return *denied, nil
	}
	if account.Role != accounts.RoleAdmin {
		return s.fail(ctx, http.StatusForbidden, appapi.ErrorCodeForbidden, nil), nil
	}
	list, err := s.accounts.List(ctx)
	if err != nil {
		return s.internalError(ctx, "列账号", err), nil
	}
	reply := appapi.ListAccounts200JSONResponse{}
	for _, each := range list {
		row, err := s.accountSummary(ctx, each)
		if err != nil {
			return s.internalError(ctx, "读账号", err), nil
		}
		reply = append(reply, row)
	}
	return reply, nil
}

// RefreshTokens 实现 appapi.StrictServerInterface。
func (s *Server) RefreshTokens(ctx context.Context, request appapi.RefreshTokensRequestObject) (appapi.RefreshTokensResponseObject, error) {
	pair, err := s.accounts.Refresh(ctx, request.Body.RefreshToken)
	if err != nil {
		status, code := accountProblem(err)
		if status == http.StatusNotFound || status == http.StatusInternalServerError {
			if status == http.StatusInternalServerError {
				return s.internalError(ctx, "刷新令牌", err), nil
			}
			status, code = http.StatusUnauthorized, appapi.ErrorCodeInvalidToken
		}
		return s.fail(ctx, status, code, nil), nil
	}
	return appapi.RefreshTokens200JSONResponse(tokenPairOf(pair)), nil
}

func tokenPairOf(pair accounts.TokenPair) appapi.TokenPair {
	return appapi.TokenPair{AccessToken: pair.AccessToken, RefreshToken: pair.RefreshToken,
		ExpiresIn: int(pair.ExpiresIn / time.Second), TokenType: appapi.Bearer}
}

// Logout 实现 appapi.StrictServerInterface：令牌本来就无效时也是 204。
func (s *Server) Logout(ctx context.Context, request appapi.LogoutRequestObject) (appapi.LogoutResponseObject, error) {
	if err := s.accounts.Revoke(ctx, request.Body.RefreshToken); err != nil {
		return s.internalError(ctx, "登出", err), nil
	}
	return appapi.Logout204Response{}, nil
}
