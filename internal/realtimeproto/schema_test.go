package realtimeproto

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
)

var updateSchema = flag.Bool("update", false, "按现在的线上结构重写 "+SchemaFile)

// 仓库里的 Schema 文件必须和线上结构一致：改了 wire.go 而没重新生成，这里失败（CI 跑测试即查漂移）。
func TestSchemaFileMatchesWireStructs(t *testing.T) {
	generated := generatedSchema(t)
	if *updateSchema {
		if err := os.WriteFile(SchemaFile, generated, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(SchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generated, committed) {
		t.Fatalf("%s 和线上结构不一致：先确认改动是否兼容（不兼容要升协议版本），再运行\n"+
			"  go test ./internal/realtimeproto -run TestSchemaFileMatchesWireStructs -update", SchemaFile)
	}
}

// 阳性对照：生成器确实读到了线上结构——每种事件都有定义、事件名写成了 const、嵌套的字段也在。
func TestSchemaCoversEveryEvent(t *testing.T) {
	var document struct {
		Defs map[string]struct {
			Properties map[string]struct {
				Const      any                        `json:"const"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
			AdditionalProperties json.RawMessage `json:"additionalProperties"`
		} `json:"$defs"`
		OneOf []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
	}
	if err := json.Unmarshal(generatedSchema(t), &document); err != nil {
		t.Fatal(err)
	}
	expect := func(side string, names []string) {
		for _, name := range names {
			definition, ok := document.Defs[side+"."+name]
			if !ok {
				t.Errorf("缺少 %s 事件 %s 的定义", side, name)
				continue
			}
			if definition.Properties["type"].Const != name {
				t.Errorf("%s 的 type 应固定为它的事件名，得到 %v", name, definition.Properties["type"].Const)
			}
			if definition.AdditionalProperties != nil {
				t.Errorf("%s 不应禁止多余字段：%s", name, definition.AdditionalProperties)
			}
		}
	}
	var clientNames, serverNames []string
	for name := range clientEvents {
		clientNames = append(clientNames, name)
	}
	for name := range serverEvents {
		serverNames = append(serverNames, name)
	}
	expect("client", clientNames)
	expect("server", serverNames)
	if len(document.OneOf) != len(clientNames)+len(serverNames) {
		t.Errorf("oneOf 应列出全部 %d 种事件，得到 %d", len(clientNames)+len(serverNames), len(document.OneOf))
	}
	if _, ok := document.Defs["client.session.update"].Properties["session"].Properties["audio"]; !ok {
		t.Error("session.update 的嵌套字段（session.audio）没有生成出来")
	}
}

func generatedSchema(t *testing.T) []byte {
	t.Helper()
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}
