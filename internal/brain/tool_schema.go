package brain

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// ParametersFor 由结构体 T 生成工具参数的定义（JSON Schema）：字段的 jsonschema 标签是给模型看的说明，
// 带 omitempty 的是可选参数。descriptions（可选）按 JSON 字段名补上说明：要用常量拼、写不进结构体标签的那几项。
// T 是代码里写死的类型，生成不了、说明对不上字段就是写错了：直接 panic，测试和启动时就暴露。
func ParametersFor[T any](descriptions ...map[string]string) json.RawMessage {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("工具参数 %v 生成不了定义: %v", reflect.TypeFor[T](), err))
	}
	for _, fields := range descriptions {
		for field, description := range fields {
			property, ok := schema.Properties[field]
			if !ok {
				panic(fmt.Sprintf("工具参数 %v 没有字段 %q，补的说明对不上", reflect.TypeFor[T](), field))
			}
			property.Description = description
		}
	}
	data, err := json.Marshal(schema)
	if err != nil {
		panic(fmt.Sprintf("工具参数 %v 的定义编不成 JSON: %v", reflect.TypeFor[T](), err))
	}
	return data
}

// DecodeArguments 按结构体 T 的定义校验模型给的参数，再解成 T：缺了必填的、类型不对都报错，不落成零值去执行。
func DecodeArguments[T any](arguments map[string]any) (T, error) {
	var decoded T
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return decoded, fmt.Errorf("参数定义: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return decoded, fmt.Errorf("参数定义: %w", err)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	// 按 JSON 的样子校验（模型给的参数本来就是 JSON 解出来的）
	raw, err := json.Marshal(arguments)
	if err != nil {
		return decoded, fmt.Errorf("参数格式不对: %w", err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return decoded, fmt.Errorf("参数格式不对: %w", err)
	}
	if err := resolved.Validate(value); err != nil {
		return decoded, fmt.Errorf("参数不对: %w", err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return decoded, fmt.Errorf("参数格式不对: %w", err)
	}
	return decoded, nil
}
