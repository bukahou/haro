// Package spending 是 haro 的用量账本与费用上限（v5 R5：先有刹车再加油门）。
//
// 大模型与联网搜索都按量计费。每次调用记一笔（token 数、搜索积分、折成日元），按天、按月汇总；
// Guard 据此决定还能不能花：后台调查按天封顶、大模型按月封顶、搜索积分按天封顶。
// 云厂商的预算告警一般只发提醒、不会停服务，真正能拦住的只有服务端自己。
//
// 对外：OpenLedger（Facade）+ Ledger（接口）；Guard；MeteredChatModel / BudgetedChatModel（装饰 brain.ChatModel）。
package spending

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动
)

// Purpose 是花钱的用途：额度按用途分开算。
type Purpose string

const (
	PurposeChat     Purpose = "chat"     // 主对话
	PurposeResearch Purpose = "research" // 后台调查（R2 / R5：按天封顶）
	PurposeSearch   Purpose = "search"   // 主对话里的快查（R1）
	PurposeSummary  Purpose = "summary"  // 滚动摘要（R4）
)

// Charge 是一笔花费。
type Charge struct {
	At           time.Time
	Purpose      Purpose
	Item         string // 花在什么上，例如 "llm:<模型 ID>"、"tavily"
	InputTokens  int
	OutputTokens int
	Credits      int     // 搜索服务的积分
	Yen          float64 // 折成日元（按配置的单价与汇率）
}

// Totals 是一段时间的合计。
type Totals struct {
	Yen     float64
	Credits int
}

// Ledger 是用量账本。
type Ledger interface {
	Record(ctx context.Context, charge Charge) error
	// Total 合计 since 之后的花费；purposes 为空 = 所有用途。
	Total(ctx context.Context, since time.Time, purposes ...Purpose) (Totals, error)
	Close() error
}

// OpenLedger 打开用量账本：path 是 SQLite 文件路径；空 = 只在内存里（服务端重启就清零，额度也跟着重置）。
func OpenLedger(ctx context.Context, path string) (Ledger, error) {
	location := "file::memory:"
	if path != "" {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("创建用量账本目录: %w", err)
			}
		}
		location = path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", location)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 单写者；内存库也只能有一个连接（否则每个连接一份）
	if _, err := db.ExecContext(ctx, ledgerSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化用量账本: %w", err)
	}
	return &sqliteLedger{db: db}, nil
}

const ledgerSchema = `
CREATE TABLE IF NOT EXISTS charges (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	at            INTEGER NOT NULL, -- Unix 秒
	purpose       TEXT    NOT NULL,
	item          TEXT    NOT NULL,
	input_tokens  INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	credits       INTEGER NOT NULL DEFAULT 0,
	yen           REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS charges_at ON charges (at);`

type sqliteLedger struct {
	db *sql.DB
}

func (l *sqliteLedger) Record(ctx context.Context, charge Charge) error {
	if charge.At.IsZero() {
		charge.At = time.Now()
	}
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO charges (at, purpose, item, input_tokens, output_tokens, credits, yen) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		charge.At.Unix(), string(charge.Purpose), charge.Item, charge.InputTokens, charge.OutputTokens, charge.Credits, charge.Yen)
	return err
}

func (l *sqliteLedger) Total(ctx context.Context, since time.Time, purposes ...Purpose) (Totals, error) {
	query := `SELECT COALESCE(SUM(yen), 0), COALESCE(SUM(credits), 0) FROM charges WHERE at >= ?`
	args := []any{since.Unix()}
	if len(purposes) > 0 {
		query += ` AND purpose IN (?` + repeat(",?", len(purposes)-1) + `)`
		for _, purpose := range purposes {
			args = append(args, string(purpose))
		}
	}
	var totals Totals
	err := l.db.QueryRowContext(ctx, query, args...).Scan(&totals.Yen, &totals.Credits)
	return totals, err
}

func (l *sqliteLedger) Close() error { return l.db.Close() }

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
