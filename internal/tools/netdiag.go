package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/netdiag"
)

// NetworkDiagnostics 是网络诊断的两个内置工具。说明里「不调用就不知道」是实测加的：去掉会让模型凭空回答。
// 它查的是服务端所在机器的网络（服务端在容器里时，查的是容器的网络）。
func NetworkDiagnostics(checker *netdiag.Checker) Builtin {
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: "check_network", Annotations: ReadOnly,
			Description: "实时检查家里网络的状态：网关延迟和丢包、DNS 解析时间、外网是否连通。主人问网络正不正常、网速怎么样、能不能上网时必须调用；网络状况每时每刻都在变，不调用就不知道，不能凭空回答。"},
			func(ctx context.Context, _ struct{}) (netdiag.Report, error) { return checker.Check(ctx), nil })
		AddTool(server, &mcp.Tool{Name: "network_interfaces", Annotations: ReadOnly,
			Description: "查看本机网卡的状态、IP 地址和链路速率（有 Wi-Fi 时含信号强度）。"},
			func(context.Context, struct{}) (interfaceList, error) {
				list, err := netdiag.Interfaces()
				return interfaceList{Interfaces: list}, err
			})
	}
}

type interfaceList struct {
	Interfaces []netdiag.Interface `json:"interfaces"`
}
