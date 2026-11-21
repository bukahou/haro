package brainclient_test

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

// logRecords 收下打出来的日志：测试按级别和属性核对，不比对日志文字。
// 被测的代码不用 With / WithGroup，这两个原样返回自己。
type logRecords struct {
	mu      sync.Mutex
	records []logRecord
}

// logRecord 是一条日志的级别和属性。
type logRecord struct {
	level  slog.Level
	fields map[string]any
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
	l.records = append(l.records, logRecord{level: record.Level, fields: fields})
	return nil
}

// matching 返回满足 match 的记录。
func (l *logRecords) matching(match func(logRecord) bool) []logRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []logRecord
	for _, record := range l.records {
		if match(record) {
			found = append(found, record)
		}
	}
	return found
}

// waitFor 等一条满足 match 的记录出现（日志可能在另一个 goroutine 里写），返回它。
func (l *logRecords) waitFor(t *testing.T, what string, match func(logRecord) bool) logRecord {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if found := l.matching(match); len(found) > 0 {
			return found[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("日志里等不到%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// hasFields 判断记录里这些属性都在、值也相同。
func (r logRecord) hasFields(want map[string]any) bool {
	for key, value := range want {
		if got, ok := r.fields[key]; !ok || !reflect.DeepEqual(got, value) {
			return false
		}
	}
	return true
}
