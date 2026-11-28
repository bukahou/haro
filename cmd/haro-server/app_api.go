package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/bukahou/haro/internal/accounts"
	"github.com/bukahou/haro/internal/appserver"
	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/brainserver"
	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/profile"
)

// entrypoints 是服务端 HTTP 入口要的东西：在线的终端（App 的概览看）、App 的接口（没开时为 nil）。
type entrypoints struct {
	online *brainserver.OnlineTerminals
	app    http.Handler
}

// tokenPruneEvery 是多久清一次早就过期的登录令牌。
const tokenPruneEvery = 24 * time.Hour

// buildAppAPI 建 App 的接口（HARO_APP_PUBLIC_URL 设了才开）：账号库、按配置校正管理员、经后端代理的 akasha 登录。
func buildAppAPI(ctx context.Context, settings config.ServerSettings, deps services, kit tooling, online *brainserver.OnlineTerminals,
	logger *slog.Logger, shutdown *shutdownSteps) (http.Handler, error) {
	if !settings.AppAPIEnabled() {
		logger.Info("App 的接口：没设 HARO_APP_PUBLIC_URL，不开")
		return nil, nil
	}
	store, err := accounts.Open(ctx, settings.AccountsDB)
	if err != nil {
		return nil, err
	}
	shutdown.add("账号库", store.Close)
	if settings.AdminSubject == "" {
		logger.Warn("App 的接口：没设 HARO_ADMIN_SUBJECT，还没有管理员：第一次登录时日志里有自己的 sub，填进配置后重启")
	} else {
		name := settings.AdminName
		if name == "" {
			name = settings.AdminPerson
		}
		admin, err := store.EnsureAdmin(ctx, accounts.Admin{Subject: settings.AdminSubject, DisplayName: name,
			Person: settings.AdminPerson, GuestPerson: brain.SharedPerson})
		if err != nil {
			return nil, err
		}
		logger.Info("App 的接口：管理员", "account", admin.ID, "person", admin.OwnerPerson)
	}
	go pruneTokens(ctx, store, logger)
	options := appserver.Options{Accounts: store, Spending: deps.guard, Terminals: online, DefaultCurrency: settings.Currency(),
		MinAppVersion: settings.AppMinVersion, CapOptions: settings.MonthlyCapOptions(), ProfileMaxChars: profile.MaxRunes,
		Logger: logger.With("component", "app-api"),
		Login: &appserver.LoginOptions{Issuer: settings.OIDCIssuer, ClientID: settings.OIDCClientID,
			ClientSecret: settings.OIDCClientSecret, CookieSecret: settings.OIDCCookieSecret, PublicURL: settings.AppPublicURL}}
	if kit.memory != nil {
		options.Memory = kit.memory
	}
	if kit.research != nil {
		options.Research = kit.research
	}
	handler, err := appserver.NewHandler(ctx, options)
	if err != nil {
		return nil, err
	}
	logger.Info("App 的接口：/api/v1/（登录经 akasha）", "public_url", settings.AppPublicURL, "issuer", settings.OIDCIssuer)
	return handler, nil
}

// pruneTokens 每天清一次早就过期的登录令牌，随 ctx 结束。
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
		}
	}
}
