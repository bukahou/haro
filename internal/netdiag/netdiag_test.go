package netdiag

import (
	"context"
	"testing"
	"time"
)

// 在本机实际跑一遍（只读：读 /proc、ping 网关、解析一个域名、建立一次 TCP 连接）。
func TestCheckOnThisMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过需要网络的测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	report := NewChecker("www.cloudflare.com", "1.1.1.1:443").Check(ctx)
	t.Logf("%+v", report)
	if report.Gateway == "" {
		t.Errorf("应找到默认网关：%s", report.GatewayError)
	}
	interfaces, err := Interfaces()
	if err != nil || len(interfaces) == 0 {
		t.Fatalf("应列出至少一块网卡：%v", err)
	}
	t.Logf("%+v", interfaces)
}

func TestDefaultGatewayParsing(t *testing.T) {
	if gateway, err := defaultGateway(); err == nil && gateway == "" {
		t.Error("解析出空网关")
	}
}
