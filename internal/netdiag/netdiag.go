// Package netdiag 是只读的网络诊断（v2.5，T4 ①）：只在本机做检查，不碰别的机器、不改任何配置。
package netdiag

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Report 是一次网络检查的结果。各字段的错误单独记录，一项失败不影响其他项。
type Report struct {
	Gateway        string  `json:"gateway"`
	GatewayLossPct float64 `json:"gateway_loss_pct"`
	GatewayAvgMs   float64 `json:"gateway_avg_ms"`
	GatewayError   string  `json:"gateway_error,omitempty"`
	DNSName        string  `json:"dns_name"`
	DNSMs          float64 `json:"dns_ms"`
	DNSError       string  `json:"dns_error,omitempty"`
	InternetTarget string  `json:"internet_target"`
	InternetMs     float64 `json:"internet_ms"`
	InternetError  string  `json:"internet_error,omitempty"`
}

// Checker 执行检查。目标地址可配置（默认是公共的 DNS 名与公共 DNS 服务器的 443 端口）。
type Checker struct {
	DNSName        string // 用来测 DNS 解析的域名
	InternetTarget string // 用来测外网连通的 host:port（只建立 TCP 连接，不发送数据）
}

// NewChecker 创建检查器。
func NewChecker(dnsName, internetTarget string) *Checker {
	return &Checker{DNSName: dnsName, InternetTarget: internetTarget}
}

// Check 依次检查网关、DNS、外网。
func (c *Checker) Check(ctx context.Context) Report {
	report := Report{DNSName: c.DNSName, InternetTarget: c.InternetTarget}

	if gateway, err := defaultGateway(); err != nil {
		report.GatewayError = err.Error()
	} else {
		report.Gateway = gateway
		report.GatewayLossPct, report.GatewayAvgMs, err = ping(ctx, gateway, 3)
		if err != nil {
			report.GatewayError = err.Error()
		}
	}

	started := time.Now()
	if _, err := net.DefaultResolver.LookupHost(ctx, c.DNSName); err != nil {
		report.DNSError = err.Error()
	} else {
		report.DNSMs = msSince(started)
	}

	started = time.Now()
	dialer := net.Dialer{Timeout: 3 * time.Second}
	if conn, err := dialer.DialContext(ctx, "tcp", c.InternetTarget); err != nil {
		report.InternetError = err.Error()
	} else {
		report.InternetMs = msSince(started)
		conn.Close()
	}
	return report
}

// Interface 是一块网卡的状态。
type Interface struct {
	Name      string   `json:"name"`
	State     string   `json:"state"`                // up / down …
	Addresses []string `json:"addresses"`            // IPv4 / IPv6（CIDR）
	SpeedMbps int      `json:"speed_mbps,omitempty"` // 有线网卡的协商速率
	WiFiLevel string   `json:"wifi_signal_dbm,omitempty"`
}

// Interfaces 列出本机的物理网卡（跳过回环与容器、隧道类虚拟网卡）。
func Interfaces() ([]Interface, error) {
	list, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	wifi := wifiSignals()
	var result []Interface
	for _, iface := range list {
		if iface.Flags&net.FlagLoopback != 0 || isVirtual(iface.Name) {
			continue
		}
		item := Interface{Name: iface.Name, State: readSys(iface.Name, "operstate"), WiFiLevel: wifi[iface.Name]}
		if speed, err := strconv.Atoi(readSys(iface.Name, "speed")); err == nil && speed > 0 {
			item.SpeedMbps = speed
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			item.Addresses = append(item.Addresses, addr.String())
		}
		result = append(result, item)
	}
	return result, nil
}

// defaultGateway 从 /proc/net/route 读默认路由的网关（Linux）。
func defaultGateway() (string, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[1] != "00000000" { // 目的地 0.0.0.0 = 默认路由
			continue
		}
		raw, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			continue
		}
		ip := net.IPv4(byte(raw), byte(raw>>8), byte(raw>>16), byte(raw>>24)) // 小端
		return ip.String(), nil
	}
	return "", fmt.Errorf("没有找到默认路由")
}

var (
	lossPattern = regexp.MustCompile(`([\d.]+)% packet loss`)
	rttPattern  = regexp.MustCompile(`= [\d.]+/([\d.]+)/`)
)

// ping 调系统的 ping（不需要特权），返回丢包率与平均延迟。
func ping(ctx context.Context, host string, count int) (lossPct, avgMs float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(count+3)*time.Second)
	defer cancel()
	// 间隔 0.2 s（普通用户允许的最小值）：默认 1 s 间隔会让一次检查等好几秒（2026-11-15 实测首句 3.8 s）
	out, runErr := exec.CommandContext(ctx, "ping", "-n", "-c", strconv.Itoa(count), "-i", "0.2", "-W", "1", host).CombinedOutput()
	text := string(out)
	if match := lossPattern.FindStringSubmatch(text); match != nil {
		lossPct, _ = strconv.ParseFloat(match[1], 64)
	} else {
		return 0, 0, fmt.Errorf("ping %s 失败: %v", host, runErr)
	}
	if match := rttPattern.FindStringSubmatch(text); match != nil {
		avgMs, _ = strconv.ParseFloat(match[1], 64)
	}
	return lossPct, avgMs, nil // 全部丢包时 ping 返回非零，但结果本身是有效的
}

// wifiSignals 从 /proc/net/wireless 读各无线网卡的信号强度（dBm）；没有无线网卡时为空。
func wifiSignals() map[string]string {
	signals := map[string]string{}
	data, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return signals
	}
	for _, line := range strings.Split(string(data), "\n")[2:] {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			signals[strings.TrimSuffix(fields[0], ":")] = strings.TrimSuffix(fields[3], ".")
		}
	}
	return signals
}

func readSys(iface, file string) string {
	data, err := os.ReadFile("/sys/class/net/" + iface + "/" + file)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func isVirtual(name string) bool {
	for _, prefix := range []string{"docker", "veth", "br-", "virbr", "cilium", "lxc", "flannel", "cni", "tun", "tap", "wg", "tailscale", "kube"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func msSince(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}
