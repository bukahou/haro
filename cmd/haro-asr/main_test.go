package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/voiceprint"
)

// logRecords 记下所有日志（测试按级别和属性核对，不看日志的文字）。
type logRecords struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) Handle(_ context.Context, record slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, record.Clone())
	return nil
}
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecords) WithGroup(string) slog.Handler      { return l }

// with 是 level 级别、带全部 keys 属性的日志条数。
func (l *logRecords) with(level slog.Level, keys ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, record := range l.records {
		found := map[string]bool{}
		record.Attrs(func(attr slog.Attr) bool {
			found[attr.Key] = true
			return true
		})
		matched := record.Level == level
		for _, key := range keys {
			matched = matched && found[key]
		}
		if matched {
			count++
		}
	}
	return count
}

// 被拒时说明里写的配置项名字取自 config.ASRSettings 的 env 标签：每个都要真是 haro-asr 的配置项（字段改了名会落空）。
func TestCapabilitySettingsComeFromConfig(t *testing.T) {
	known := map[string]bool{}
	settings := reflect.TypeFor[config.ASRSettings]()
	for i := range settings.NumField() {
		name, _, _ := strings.Cut(settings.Field(i).Tag.Get("env"), ",")
		known[name] = true
	}
	capabilities := capabilitySettings()
	for capability, names := range map[string]string{"VAD": capabilities.VoiceDetector, "说完判断": capabilities.TurnDetector,
		"唤醒词": capabilities.KeywordSpotter} {
		for _, name := range strings.Split(names, "、") {
			if name == "" || !known[name] {
				t.Errorf("%s 的配置项 %q 不是 haro-asr 的配置项", capability, name)
			}
		}
	}
}

const model = "campplus.onnx"

// 声纹目录还不存在（第一次登记之前）：建出来，照「还没有人登记」报。
func TestLoadVoiceprintsCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "voiceprints")
	logs := &logRecords{}
	voiceprints, err := loadVoiceprints(config.ASRSettings{VoiceprintsDir: dir}, model, slog.New(logs))
	if err != nil || len(voiceprints) != 0 {
		t.Fatalf("目录还没有时应建出来、照常启动：%v %v", voiceprints, err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("应建好声纹目录（0700）：%v %v", info, err)
	}
	if logs.with(slog.LevelWarn, "dir") != 1 || logs.with(slog.LevelWarn, "unusable") != 0 {
		t.Error("应照「还没有人登记」报一条警告")
	}
}

// 登记过的声纹都是用别的模型算的：逐份警告（写明模型），总的那句写明是都用不上，不说成「还没有人登记」。
// 只有一部分用不上时，用得上的照常用，用不上的同样逐份警告。
func TestLoadVoiceprintsReportsUnusable(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"主人", "太太"} {
		if _, err := voiceprint.Save(dir, voiceprint.Voiceprint{Name: name, Model: "old.onnx", Embeddings: [][]float32{{1, 0}}}); err != nil {
			t.Fatal(err)
		}
	}
	logs := &logRecords{}
	voiceprints, err := loadVoiceprints(config.ASRSettings{VoiceprintsDir: dir}, model, slog.New(logs))
	if err != nil || len(voiceprints) != 0 {
		t.Fatalf("都用不上：%v %v", voiceprints, err)
	}
	if got := logs.with(slog.LevelWarn, "file", "model"); got != 2 {
		t.Errorf("每份用不上的声纹各警告一条（写明模型）：%d 条", got)
	}
	if logs.with(slog.LevelWarn, "unusable") != 1 {
		t.Error("总的那句应写明登记过的都用不上（带 unusable），不说成还没有人登记")
	}

	if _, err := voiceprint.Save(dir, voiceprint.Voiceprint{Name: "主人", Model: model, Embeddings: [][]float32{{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	logs = &logRecords{}
	voiceprints, err = loadVoiceprints(config.ASRSettings{VoiceprintsDir: dir}, model, slog.New(logs))
	if err != nil || len(voiceprints) != 1 || voiceprints[0].Name != "主人" {
		t.Fatalf("用得上的照常用：%v %v", voiceprints, err)
	}
	if logs.with(slog.LevelWarn, "file", "model") != 1 || logs.with(slog.LevelInfo, "speakers", "unusable") != 1 {
		t.Error("用不上的那份照样警告；启用的那句带上认得谁、几份用不上")
	}
}

// 配了目录却读不了（没权限）：报错，haro-asr 起不来，不当成「还没有人登记」。
func TestLoadVoiceprintsUnreadableDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("当前用户读得了没有权限的目录（root？），测不了")
	}
	if _, err := loadVoiceprints(config.ASRSettings{VoiceprintsDir: dir}, model, slog.New(&logRecords{})); err == nil {
		t.Error("声纹目录读不了应报错")
	}
}
