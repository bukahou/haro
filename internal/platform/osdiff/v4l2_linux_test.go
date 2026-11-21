//go:build linux

package osdiff

import (
	"testing"

	"github.com/blackjack/webcam"
)

// 帧间隔是 numerator/denominator 秒：1/30 秒 = 30 帧；取最高的那个。
func TestHighestFPS(t *testing.T) {
	rates := []webcam.FrameRate{{MinNumerator: 1, MinDenominator: 10}, {MinNumerator: 1, MinDenominator: 30}, {MinNumerator: 2, MinDenominator: 30}}
	if got := highestFPS(rates); got != 30 {
		t.Errorf("应为 30：%v", got)
	}
	if got := highestFPS(nil); got != 0 {
		t.Errorf("没有帧率时为 0：%v", got)
	}
}
