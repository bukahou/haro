package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bukahou/haro/internal/config"
	"github.com/bukahou/haro/internal/research"
	"github.com/bukahou/haro/internal/speech"
)

// 调查的上限：配置没设（0）的用程序里的默认值（research.DefaultLimits），设了的用配置的。
func TestResearchLimits(t *testing.T) {
	if got := researchLimits(config.ServerSettings{}); got != research.DefaultLimits {
		t.Errorf("都没设时应是 research.DefaultLimits：%+v", got)
	}
	got := researchLimits(config.ServerSettings{ResearchTimeout: time.Minute, ResearchMaxRounds: 4, ResearchMaxSearches: 2, ResearchMaxTokens: 1000})
	want := research.DefaultLimits
	want.Timeout, want.MaxRounds, want.MaxSearches, want.MaxTokens = time.Minute, 4, 2, 1000
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
