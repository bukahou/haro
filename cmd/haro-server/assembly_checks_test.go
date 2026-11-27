package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/speech"
)

// warnRecorder 收下警告级以上的日志记录（按记录的属性判断，不看日志文字）。
type warnRecorder struct{ records *[]slog.Record }

func (h warnRecorder) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelWarn
}
func (h warnRecorder) Handle(_ context.Context, record slog.Record) error {
	*h.records = append(*h.records, record)
	return nil
}
func (h warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h warnRecorder) WithGroup(string) slog.Handler      { return h }

// 关停时各步后开的先关；出错的不吞掉：全部做完后合成一行警告，写明几步、各是什么错。
func TestShutdownStepsReportFailures(t *testing.T) {
	var order []string
	var steps shutdownSteps
	steps.add("对话库", func() error { order = append(order, "对话库"); return errors.New("WAL 没合并") })
	steps.add("工具箱", func() error { order = append(order, "工具箱"); return nil })
	steps.add("遥测", func() error { order = append(order, "遥测"); return errors.New("采集器连不上") })
	var records []slog.Record
	steps.run(slog.New(warnRecorder{records: &records}))
	if !slices.Equal(order, []string{"遥测", "工具箱", "对话库"}) {
		t.Errorf("后开的先关：%v", order)
	}
	if len(records) != 1 {
		t.Fatalf("出错的几步合成一行警告，得到 %d 行", len(records))
	}
	var count int64
	var joined error
	records[0].Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "steps":
			count = attr.Value.Int64()
		case "err":
			joined, _ = attr.Value.Any().(error)
		}
		return true
	})
	if count != 2 || joined == nil || !strings.Contains(joined.Error(), "对话库") || !strings.Contains(joined.Error(), "遥测") {
		t.Errorf("警告里应写明两步出错：steps=%d err=%v", count, joined)
	}
	records = nil
	var clean shutdownSteps
	clean.add("工具箱", func() error { return nil })
	clean.run(slog.New(warnRecorder{records: &records}))
	if len(records) != 0 {
		t.Errorf("都没出错时不记警告：%d", len(records))
	}
}

// 调查的上限：配置没设（0）的用程序里的默认值（research.DefaultLimits），设了的用配置的（轮数见 TestBuildResearchAgents）。
func TestResearchLimits(t *testing.T) {
	if got := researchLimits(config.ServerSettings{}); got != research.DefaultLimits {
		t.Errorf("都没设时应是 research.DefaultLimits：%+v", got)
	}
	got := researchLimits(config.ServerSettings{ResearchTimeout: time.Minute, ResearchMaxSearches: 2, ResearchMaxTokens: 1000,
		ResearchMaxCallTokens: 500, ResearchWriteUpTimeout: 2 * time.Minute, ResearchRecheckTimeout: 3 * time.Minute})
	want := research.DefaultLimits
	want.Timeout, want.MaxSearches, want.MaxTokens, want.MaxCallTokens = time.Minute, 2, 1000, 500
	want.WriteUpTimeout, want.RecheckTimeout = 2*time.Minute, 3*time.Minute
	if got != want {
		t.Errorf("设了的用配置的：%+v", got)
	}
}

// 默认性格写错（不在性格档案里）时起不来，不悄悄换成档案里的第一个。
func TestDefaultPersonaMustExist(t *testing.T) {
	personas := filepath.Join("..", "..", "assets", "personas")
	if _, err := buildBrainOptions(config.ServerSettings{PersonasDir: personas, PersonaDefault: "buddy"}); err != nil {
		t.Fatalf("档案里有的性格应可以：%v", err)
	}
	_, err := buildBrainOptions(config.ServerSettings{PersonasDir: personas, PersonaDefault: "budy"})
	if err == nil || !strings.Contains(err.Error(), "HARO_PERSONA_DEFAULT") {
		t.Errorf("档案里没有的默认性格应起不来并写明 HARO_PERSONA_DEFAULT：%v", err)
	}
}

// 声音档案里参考录音的名字要么是平常语气那段，要么在情绪清单里；拼错的起不来。
func TestVoiceEmotionsMustBeKnown(t *testing.T) {
	reference := speech.GPTSoVITSReference{File: "ref.wav", Text: "你好", Lang: "zh"}
	good := speech.GPTSoVITSVoice{References: map[string]speech.GPTSoVITSReference{speech.DefaultReference: reference,
		"tease": reference, "gentle": reference}}
	if err := checkVoiceEmotions(good, "voice.json"); err != nil {
		t.Errorf("default 和清单里的情绪应可以：%v", err)
	}
	example, err := speech.LoadGPTSoVITSVoice(filepath.Join("..", "..", "assets", "voices", "example.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkVoiceEmotions(example, "example.json"); err != nil {
		t.Errorf("仓库里的示例声音档案应可以：%v", err)
	}
	for _, bad := range []string{"teese", "调侃", ""} {
		voice := speech.GPTSoVITSVoice{References: map[string]speech.GPTSoVITSReference{speech.DefaultReference: reference, bad: reference}}
		if err := checkVoiceEmotions(voice, "voice.json"); err == nil || !strings.Contains(err.Error(), "voice.json") {
			t.Errorf("参考录音名 %q 不在情绪清单里，应起不来：%v", bad, err)
		}
	}
}
