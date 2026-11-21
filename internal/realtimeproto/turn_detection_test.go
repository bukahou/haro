package realtimeproto

import "testing"

// 说完方式、积极度、阈值、毫秒数的取值只在 TurnDetection.Validate 定义：合法的通过，写错的报错。
func TestTurnDetectionValidate(t *testing.T) {
	for _, ok := range []TurnDetection{{Type: TurnServerVAD}, {Type: TurnSemanticVAD, Eagerness: EagernessHigh},
		{Type: TurnSemanticVAD, Eagerness: EagernessMedium, Threshold: 0.5, PrefixPaddingMs: 300, SilenceDurationMs: 700}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v 应通过：%v", ok, err)
		}
	}
	for _, bad := range []TurnDetection{{Type: ""}, {Type: "vad"}, {Type: TurnSemanticVAD, Eagerness: "fast"},
		{Type: TurnServerVAD, Threshold: 1.5}, {Type: TurnServerVAD, SilenceDurationMs: -1}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v 应报错", bad)
		}
	}
}
