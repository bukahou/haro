package brain_test

import (
	"encoding/json"
	"testing"

	"github.com/bukahou/haro/internal/brain"
)

type describedInput struct {
	Query string `json:"query" jsonschema:"搜索词"`
	Days  int    `json:"days,omitempty"`
}

// 写不进结构体标签的说明（要用常量拼的）由 ParametersFor 的可选参数按字段名补上；标签里的照旧；
// 说明对不上字段时直接 panic（写错了，启动和测试时就暴露），不悄悄丢掉。
func TestParametersForFillsDescriptions(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Description string `json:"description"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(brain.ParametersFor[describedInput](map[string]string{"days": "1–7 天"}), &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["days"].Description != "1–7 天" || schema.Properties["query"].Description != "搜索词" {
		t.Errorf("说明：%+v", schema.Properties)
	}
	defer func() {
		if recover() == nil {
			t.Error("说明对不上字段时应 panic")
		}
	}()
	brain.ParametersFor[describedInput](map[string]string{"day": "写错了字段名"})
}
