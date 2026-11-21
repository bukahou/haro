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
	PurposeVision   Purpose = "vision"   // 看画面的幕后专家（与主对话分开记，看得出看画面花了多少）
	PurposeStrong   Purpose = "strong"   // 更强的模型接手回答（主人纠正时、看画面接手时）
)

// ParsePurpose 把代理定义里写的用途换成 Purpose；不认识的报错（写错了就起不来，费用不会记到一个没人看的用途上）。
func ParsePurpose(name string) (Purpose, error) {
	for _, purpose := range []Purpose{PurposeChat, PurposeResearch, PurposeSearch, PurposeSummary, PurposeVision, PurposeStrong} {
		if name == string(purpose) {
			return purpose, nil
		}
	}
	return "", fmt.Errorf("不认识的记账用途 %q（chat、research、search、summary、vision、strong）", name)
}

// Charge 是一笔花费。
type Charge struct {
	At           time.Time
	Purpose      Purpose
	Item         string // 花在什么上，例如 "llm:<模型 ID>"、"tavily"
	InputTokens  int    // 输入的总数（含缓存读、缓存写）
	OutputTokens int
	// 提示词缓存：输入里有多少是读缓存 / 写缓存的（价格不同；看缓存效果也靠它们）
	CacheReadTokens  int
	CacheWriteTokens int
	Credits          int     // 搜索服务的积分
	Yen              float64 // 折成日元（按配置的单价与汇率；不完整时只按知道的部分算）
	// Incomplete：调用被打断或出错，用量只知道一部分：输出数不知道（账本里记 NULL）。
	// TokensUnknown：连输入也不知道（还没开始就失败了），各项 token 都记 NULL。不知道的不编成 0
	Incomplete    bool
	TokensUnknown bool
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
	// CheckWritable 确认账本现在写得进去（就绪检查用）：不改任何数据。
	CheckWritable(ctx context.Context) error
	// RaiseCap 记下一次主人确认的临时提高上限；CapRaised 合计 since 之后提高了多少日元。
	RaiseCap(ctx context.Context, raise CapRaise) error
	CapRaised(ctx context.Context, since time.Time) (float64, error)
	Close() error
}

// CapRaise 是主人口头确认的一次临时提高上限。
type CapRaise struct {
	At     time.Time // 零值 = 现在
	Yen    float64
	Person string // 谁确认的（对话名）
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
	if err := migrateLedger(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("升级用量账本的表结构: %w", err)
	}
	return &sqliteLedger{db: db}, nil
}

type sqliteLedger struct {
	db *sql.DB
}

func (l *sqliteLedger) Record(ctx context.Context, charge Charge) error {
	if charge.At.IsZero() {
		charge.At = time.Now()
	}
	known := func(tokens int, unknown bool) sql.NullInt64 {
		return sql.NullInt64{Int64: int64(tokens), Valid: !unknown}
	}
	inputUnknown := charge.TokensUnknown
	outputUnknown := charge.TokensUnknown || charge.Incomplete
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO charges (at, purpose, item, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, credits, yen, incomplete)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		charge.At.Unix(), string(charge.Purpose), charge.Item, known(charge.InputTokens, inputUnknown),
		known(charge.OutputTokens, outputUnknown), known(charge.CacheReadTokens, inputUnknown),
		known(charge.CacheWriteTokens, inputUnknown), charge.Credits, charge.Yen, charge.Incomplete)
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

// CheckWritable 开一个写事务（拿到写锁）再回滚：磁盘只读、库文件被锁住时会出错，数据不变。
func (l *sqliteLedger) CheckWritable(ctx context.Context) error {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("账本写不进去: %w", err)
	}
	// 回滚不受 ctx 影响：拿到写锁后一定要放掉
	_, err = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	return err
}

func (l *sqliteLedger) RaiseCap(ctx context.Context, raise CapRaise) error {
	if raise.At.IsZero() {
		raise.At = time.Now()
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO cap_raises (at, yen, person) VALUES (?, ?, ?)`, raise.At.Unix(), raise.Yen, raise.Person)
	return err
}

func (l *sqliteLedger) CapRaised(ctx context.Context, since time.Time) (float64, error) {
	var yen float64
	err := l.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(yen), 0) FROM cap_raises WHERE at >= ?`, since.Unix()).Scan(&yen)
	return yen, err
}

func (l *sqliteLedger) Close() error { return l.db.Close() }

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
