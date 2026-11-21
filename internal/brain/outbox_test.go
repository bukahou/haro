package brain

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/bukahou/haro/internal/realtimeproto"
)

// 可以丢的事件（提示）在终端积压太多时丢掉并计数；一定要送到的不走这条路。
func TestDroppedNoticesAreCounted(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	session := NewSession(Components{}, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer session.Close()
	notice := realtimeproto.HaroNotice{Kind: realtimeproto.NoticeResearchDone}
	for i := range cap(session.events) {
		if !session.tryEmit(notice) {
			t.Fatalf("第 %d 个：队列没满时应送进去", i+1)
		}
	}
	if session.tryEmit(notice) {
		t.Fatal("队列满了：应丢掉")
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	if got := droppedCount(collected); got != 1 {
		t.Errorf("丢掉的应计数 1 次，实际 %d", got)
	}
}

func droppedCount(collected metricdata.ResourceMetrics) int64 {
	var total int64
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "haro.events.dropped" {
				for _, point := range sum.DataPoints {
					total += point.Value
				}
			}
		}
	}
	return total
}
