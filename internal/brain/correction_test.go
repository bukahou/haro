package brain

import "testing"

func TestIsCorrection(t *testing.T) {
	cases := map[string]bool{
		"不对，那是耳机盒": true,
		"你说错了":     true,
		"不是这个，再看看": true,
		"错了。":      true,
		"再想想":      true,
		"違うよ":      true,
		"这样对不对？":   false, // 在问对不对，不是纠正
		"我觉得身体不对劲": false,
		"今天天气怎么样":  false,
		"没错了，就是它":  false,
		"这样做错了吗":   false,
	}
	for text, want := range cases {
		if got := isCorrection(text); got != want {
			t.Errorf("%q：得到 %v，应为 %v", text, got, want)
		}
	}
}
