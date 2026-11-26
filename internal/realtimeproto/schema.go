package realtimeproto

import (
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
)

// Protocol 是 haro 对话会话现在的协议版本，即 WebSocket 子协议名。只有不兼容的改动才升版本
// （v3：终端记的醒 / 睡要等 haro.wake_word.state，v2 的服务端不发它；
// v4：服务端会在两轮之间自己开口（response.created 带 haro.unprompted），v3 的终端不在那时播；
// 唤醒词由服务端在 session.updated 里给，v3 的终端用自己配的）。
const Protocol = "haro.realtime.v4"

// SchemaFile 是仓库里由 Schema 生成的文件（相对本包目录）；测试比对它和线上结构，不一致就失败。
const SchemaFile = Protocol + ".schema.json"

// Schema 由线上结构（wire.go）和编解码的两张表（codec.go）生成协议的 JSON Schema：
// 每种事件一个定义（$defs 里的 client.<事件名>、server.<事件名>），根上是全部事件的 oneOf。
// 对象一律允许多出来的字段：解码时不认识的字段忽略（对端比自己新时照样能用）。
func Schema() (*jsonschema.Schema, error) {
	root := &jsonschema.Schema{
		Schema:      "https://json-schema.org/draft/2020-12/schema",
		Title:       Protocol,
		Description: "haro 的事件：终端 ↔ 服务端的对话会话、服务端 ↔ haro-asr 的转写会话。OpenAI Realtime 正式版的子集加 haro 扩展。",
		Defs:        map[string]*jsonschema.Schema{},
	}
	add := func(side, name string, wire any) error {
		schema, err := jsonschema.ForType(reflect.TypeOf(wire), &jsonschema.ForOptions{TypeSchemas: wireTypeSchemas})
		if err != nil {
			return fmt.Errorf("%s 事件 %s: %w", side, name, err)
		}
		typeField, ok := schema.Properties["type"]
		if !ok {
			return fmt.Errorf("%s 事件 %s 的线上结构没有 type 字段", side, name)
		}
		var eventName any = name
		typeField.Const = &eventName
		allowExtraFields(schema)
		key := side + "." + name
		root.Defs[key] = schema
		root.OneOf = append(root.OneOf, &jsonschema.Schema{Ref: "#/$defs/" + key})
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(clientEvents)) {
		if err := add("client", name, clientEvents[name].wire); err != nil {
			return nil, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(serverEvents)) {
		if err := add("server", name, serverEvents[name].wire); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// wireTypeSchemas 是自己写了 JSON 编解码的线上类型的 Schema（生成器按 Go 结构推不出来）。
var wireTypeSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[millisField](): {Type: "integer", Minimum: new(0.0)},
}

// allowExtraFields 去掉生成器给对象加的「不许有别的字段」。
func allowExtraFields(schema *jsonschema.Schema) {
	if schema == nil {
		return
	}
	if len(schema.Properties) > 0 {
		schema.AdditionalProperties = nil
	}
	for _, property := range schema.Properties {
		allowExtraFields(property)
	}
	allowExtraFields(schema.Items)
}
