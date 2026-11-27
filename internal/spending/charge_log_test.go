package spending

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
)

// logRecords 收下打出来的日志（按字段和级别核对，不比对文字）。
type logRecords struct {
	mu      sync.Mutex
	records []map[string]any
	levels  []slog.Level // 和 records 一一对应
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRecords) WithGroup(string) slog.Handler            { return l }
func (l *logRecords) Handle(_ context.Context, record slog.Record) error {
	fields := map[string]any{}
	record.Attrs(func(attr slog.Attr) bool {
		fields[attr.Key] = attr.Value.Any()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, fields)
	l.levels = append(l.levels, record.Level)
	return nil
}

// withField 返回带 key 这个字段的日志（和它们的级别）。
func (l *logRecords) withField(key string) ([]map[string]any, []slog.Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]any
	var levels []slog.Level
	for i, fields := range l.records {
		if _, ok := fields[key]; ok {
			found = append(found, fields)
			levels = append(levels, l.levels[i])
		}
	}
	return found, levels
}

// failingLedger 什么都记不上。
type failingLedger struct{ Ledger }

func (failingLedger) Record(context.Context, Charge) error { return errors.New("磁盘满了") }

// 每记上一笔都打一行：完整的、被打断的（输出数不知道）、一点用量都没拿到的、搜索积分，各一行；
// 不知道的数不写；没记上的不打（调用方会说明）。
func TestChargeLogWritesOneLinePerCharge(t *testing.T) {
	ctx := context.Background()
	inner, err := OpenLedger(ctx, filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	logs := &logRecords{}
	ledger := &loggingLedger{Ledger: inner, logger: slog.New(logs)}

	charges := []Charge{
		{Purpose: PurposeChat, Item: "llm:haiku", InputTokens: 7000, OutputTokens: 98, CacheReadTokens: 6900, Cost: jpy(1.2)},
		{Purpose: PurposeChat, Item: "llm:haiku", InputTokens: 7100, CacheReadTokens: 7000, Cost: jpy(0.9), Incomplete: true},
		{Purpose: PurposeChat, Item: "llm:haiku", Incomplete: true, TokensUnknown: true},
		{Purpose: PurposeResearch, Item: "tavily", Credits: 2, Cost: jpy(3)},
	}
	for _, charge := range charges {
		if err := ledger.Record(ctx, charge); err != nil {
			t.Fatal(err)
		}
	}
	if err := (&loggingLedger{Ledger: failingLedger{}, logger: slog.New(logs)}).Record(ctx, charges[0]); err == nil {
		t.Fatal("没记上应把错误交给调用方")
	}

	if len(logs.records) != len(charges) {
		t.Fatalf("记上 %d 笔应有 %d 行日志（没记上的不打），实际 %d 行", len(charges), len(charges), len(logs.records))
	}
	complete, interrupted, unknown, search := logs.records[0], logs.records[1], logs.records[2], logs.records[3]
	if complete["output_tokens"] != int64(98) || complete["input_tokens"] != int64(7000) || complete["incomplete"] != nil {
		t.Errorf("完整的一笔：%v", complete)
	}
	if interrupted["incomplete"] != true || interrupted["input_tokens"] != int64(7100) || interrupted["output_tokens"] != nil {
		t.Errorf("被打断的一笔：要标不完整、带输入数、不写输出数（不知道）：%v", interrupted)
	}
	if unknown["incomplete"] != true || unknown["tokens_unknown"] != true || unknown["input_tokens"] != nil {
		t.Errorf("一点用量都没拿到的一笔：不写任何 token 数：%v", unknown)
	}
	if search["credits"] != int64(2) || search["input_tokens"] != nil {
		t.Errorf("搜索的一笔：写积分：%v", search)
	}
}
