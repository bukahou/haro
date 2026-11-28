package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appserver"
	"github.com/bukahou/haro/internal/attachments"
	"github.com/bukahou/haro/internal/backstory"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/chat"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/expenses"
	"github.com/bukahou/haro/internal/household"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/profile"
)

// entrypoints 是服务端 HTTP 入口要的东西：在线的终端（App 的概览看）、App 的接口（没开时为 nil）。
type entrypoints struct {
	online   *brainserver.OnlineTerminals
	app      http.Handler
	accounts *accounts.Store // 账号库（终端的登记、设备凭证）；App 的接口没开时为 nil
}

// tokenPruneEvery 是多久清一次早就过期的登录令牌。
const tokenPruneEvery = 24 * time.Hour

// appAccounts 是 App 接口的账号库和管理员（App 的接口没开时都是空的）。在装工具、大脑之前打开：
// 主人资料、家的位置按账号取（internal/household），管理员先按配置校正好。
type appAccounts struct {
	store *accounts.Store // nil = App 的接口没开
	admin accounts.Account
}

// household 是「家」那一层要知道的管理员（还没有管理员时为空）。
func (a appAccounts) household() household.Admin {
	if a.admin.ID == "" {
		return household.Admin{}
	}
	return household.Admin{AccountID: a.admin.ID, OwnerPerson: a.admin.OwnerPerson, GuestPerson: a.admin.GuestPerson}
}

// openAccounts 打开账号库（HARO_APP_PUBLIC_URL 设了才开）、按配置校正管理员。
func openAccounts(ctx context.Context, settings config.ServerSettings, logger *slog.Logger, shutdown *shutdownSteps) (appAccounts, error) {
	if !settings.AppAPIEnabled() {
		return appAccounts{}, nil
	}
	store, err := accounts.Open(ctx, settings.AccountsDB)
	if err != nil {
		return appAccounts{}, err
	}
	shutdown.add("账号库", store.Close)
	opened := appAccounts{store: store}
	if settings.AdminSubject == "" {
		logger.Warn("App 的接口：没设 HARO_ADMIN_SUBJECT，还没有管理员：第一次登录时日志里有自己的 sub，填进配置后重启")
		return opened, nil
	}
	name := settings.AdminName
	if name == "" {
		name = settings.AdminPerson
	}
	if opened.admin, err = store.EnsureAdmin(ctx, accounts.Admin{Subject: settings.AdminSubject, DisplayName: name,
		Person: settings.AdminPerson, GuestPerson: brain.SharedPerson}); err != nil {
		return appAccounts{}, err
	}
	logger.Info("App 的接口：管理员", "account", opened.admin.ID, "person", opened.admin.OwnerPerson)
	return opened, nil
}

// memoryRecall 把长期记忆接到打字对话的搜索：按意思找到的轮次编号（没开按意思找时只给最近记的事，没有轮次）。
type memoryRecall struct{ keeper *memory.Keeper }

// RecallTurns 实现 chat.Recaller。
func (m memoryRecall) RecallTurns(ctx context.Context, person, query string) ([]int64, error) {
	recollection, err := m.keeper.Recall(ctx, person, query)
	if err != nil {
		return nil, err
	}
	seqs := make([]int64, 0, len(recollection.Turns))
	for _, turn := range recollection.Turns {
		seqs = append(seqs, turn.Seq)
	}
	return seqs, nil
}

// appBrain 是 App 的打字对话要用的大脑（和终端的会话同一套部件）。
type appBrain struct {
	components brain.Components
	options    brain.Options
	title      *brain.Agent       // 起标题；nil = 不起
	receipt    *brain.Agent       // 记账时读小票；nil = 没有记账
	backstory  *backstory.Service // 背景记录池；nil = 没有
}

// textSessions 为打字对话开会话：同一套部件，只发文字、接在指定的线程、不用摄像头；别的账号只认本人（同终端）；
// 这一轮里发生的事（工具在做什么、卡片）交给 activity。
func (b appBrain) textSessions(logger *slog.Logger) chat.SessionFactory {
	return func(speaker chat.Speaker, thread string, activity brain.ActivityObserver) chat.TextSession {
		parts := b.components
		parts.TextOnly, parts.Thread, parts.Camera, parts.Residents, parts.Activity = true, thread, nil, brain.Residents{}, activity
		if !speaker.Admin {
			parts.Residents = brain.Residents{Owner: speaker.Owner, Guest: speaker.Guest}
		}
		return brain.NewSession(parts, b.options, logger.With("component", "chat-session"))
	}
}

// buildAppAPI 建 App 的接口（HARO_APP_PUBLIC_URL 设了才开）：经后端代理的 akasha 登录、各接口。
func buildAppAPI(ctx context.Context, settings config.ServerSettings, deps services, kit tooling, assembled appBrain,
	online *brainserver.OnlineTerminals, logger *slog.Logger, shutdown *shutdownSteps) (http.Handler, error) {
	store := deps.accounts.store
	if store == nil {
		logger.Info("App 的接口：没设 HARO_APP_PUBLIC_URL，不开")
		return nil, nil
	}
	go pruneTokens(ctx, store, logger)
	options := appserver.Options{Accounts: store, Admin: store, Spending: deps.guard, Terminals: online, Registry: store, DefaultCurrency: settings.Currency(),
		MinAppVersion: settings.AppMinVersion, CapOptions: settings.MonthlyCapOptions(), ProfileMaxChars: profile.MaxRunes,
		Household: kit.household, WebURL: settings.AppWebURL, Phrases: assembled.options.Phrases, Logger: logger.With("component", "app-api"),
		Login: &appserver.LoginOptions{Issuer: settings.OIDCIssuer, ClientID: settings.OIDCClientID,
			ClientSecret: settings.OIDCClientSecret, CookieSecret: settings.OIDCCookieSecret, PublicURL: settings.AppPublicURL}}
	if kit.places != nil {
		options.Places = kit.places
	}
	var images *attachments.Files // App 里发的图片、小票的原图（同一个目录）
	if settings.AttachmentsDir != "" {
		var err error
		if images, err = attachments.OpenFiles(settings.AttachmentsDir); err != nil {
			return nil, err
		}
	}
	if deps.conversations != nil { // 打字对话要对话库（线程、完整记录）
		chatOptions := chat.Options{Store: deps.conversations, Sessions: assembled.textSessions(logger), Logger: logger}
		if assembled.components.Conversations != nil { // 确认卡片还能不能点：看对话簿里那次操作还在不在等确认
			chatOptions.Actions = assembled.components.Conversations
		}
		if assembled.title != nil {
			chatOptions.Titler = chat.AgentTitler{Agent: *assembled.title}
		}
		if kit.memory != nil {
			chatOptions.Recall = memoryRecall{keeper: kit.memory}
		}
		if images != nil {
			chatOptions.Images = images
		} else {
			logger.Info("App 的打字对话：没设 HARO_ATTACHMENTS_DIR，不收图片")
		}
		options.Chat = chat.NewService(ctx, chatOptions)
	}
	personData := kit.personData
	if deps.conversations != nil {
		personData = append(personData, accounts.NamedData{Name: dataConversations,
			Data: conversationEraser{book: assembled.components.Conversations, store: deps.conversations, images: images, logger: logger}})
	}
	if kit.memory != nil {
		options.Memory = kit.memory
	}
	if assembled.backstory != nil {
		options.Backstory = assembled.backstory
	}
	if kit.calendar != nil {
		options.Calendar = kit.calendar
	}
	if kit.research != nil {
		options.Research = kit.research
	}
	if settings.ExpensesDB != "" && assembled.receipt != nil { // 记账（配置校验保证了有存原图的目录）
		books, err := expenses.OpenStore(ctx, settings.ExpensesDB)
		if err != nil {
			return nil, err
		}
		shutdown.add("账目的库", books.Close)
		// 按月汇总的「月」按服务端的时区（和提醒、日历一样：time.Local，即 TZ）
		service := expenses.NewService(ctx, expenses.Options{Store: books, Images: images,
			Parser: expenses.AgentParser{Agent: *assembled.receipt}, Logger: logger})
		go service.Run(ctx)
		options.Expenses, options.Rates = service, deps.rates
		personData = append(personData, accounts.NamedData{Name: dataExpenses, Data: service})
		logger.Info("记账：交小票由代理读成草稿，主人确认才入账", "db", settings.ExpensesDB)
	}
	personData = append(personData, accounts.NamedData{Name: dataVoiceprints,
		Data: voiceprintEraser{asrURL: settings.ASRURL, client: deps.httpClient}})
	go accounts.Purger{Store: store, Data: personData, Logger: logger.With("component", "account-purge"),
		IdleAnonymousAfter: time.Duration(settings.AnonymousIdleDays) * 24 * time.Hour}.Run(ctx)
	handler, err := appserver.NewHandler(ctx, options)
	if err != nil {
		return nil, err
	}
	logger.Info("App 的接口：/api/v1/（登录经 akasha）", "public_url", settings.AppPublicURL, "issuer", settings.OIDCIssuer,
		"web_url", settings.AppWebURL)
	return handler, nil
}

// pruneTokens 每天清一次早就过期的登录令牌、终端不再来问的登记，随 ctx 结束。
func pruneTokens(ctx context.Context, store *accounts.Store, logger *slog.Logger) {
	ticker := time.NewTicker(tokenPruneEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.PruneTokens(ctx); err != nil {
				logger.Warn("清过期的登录令牌失败", "err", err)
			}
			if err := store.PruneRegistrations(ctx); err != nil {
				logger.Warn("清过期的终端登记失败", "err", err)
			}
		}
	}
}
