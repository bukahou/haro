package tools

import (
	"context"
	"encoding/json"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/netdiag"
)

// Builtin 是大脑进程里直接执行的工具（不走 MCP）。
type Builtin struct {
	Spec brain.ToolSpec
	Run  func(context.Context, map[string]any) (string, error)
}

// NetworkDiagnostics 返回网络诊断的两个内置工具（v3 V-4：内置到主程序，不再是 haro-netdiag 子进程）。
// 说明文字沿用 v2.5 的 haro-netdiag（已删）：「不调用就不知道」那句是实测加的，去掉会让模型凭空回答。
// 它查的是大脑所在机器的网络（大脑在容器里时，查的是容器的网络）。
func NetworkDiagnostics(checker *netdiag.Checker) []Builtin {
	noParameters := json.RawMessage(`{"type":"object","properties":{}}`)
	return []Builtin{
		{Spec: brain.ToolSpec{Name: "check_network", Parameters: noParameters,
			Description: "实时检查家里网络的状态：网关延迟和丢包、DNS 解析时间、外网是否连通。主人问网络正不正常、网速怎么样、能不能上网时必须调用；网络状况每时每刻都在变，不调用就不知道，不能凭空回答。"},
			Run: func(ctx context.Context, _ map[string]any) (string, error) {
				return marshalResult(checker.Check(ctx))
			}},
		{Spec: brain.ToolSpec{Name: "network_interfaces", Parameters: noParameters,
			Description: "查看本机网卡的状态、IP 地址和链路速率（有 Wi-Fi 时含信号强度）。"},
			Run: func(context.Context, map[string]any) (string, error) {
				list, err := netdiag.Interfaces()
				if err != nil {
					return "", err
				}
				return marshalResult(map[string]any{"interfaces": list})
			}},
	}
}

func marshalResult(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}
