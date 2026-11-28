package brainserver

import (
	"sync"

	"github.com/bukahou/haro/internal/buildinfo"
)

// OnlineTerminals 记着现在连着的 haro 终端（连上时加、断开时去掉）：App 的概览、终端列表看在线几台、哪几台、要不要升级；
// 在 App 里移除终端时把它正连着的会话断开。没带设备凭证的终端（过渡期）没有终端编号。
type OnlineTerminals struct {
	mu        sync.Mutex
	next      int
	terminals map[int]onlineTerminal // 连接编号 → 这条连接
}

type onlineTerminal struct {
	version    string
	terminalID string // 空 = 没带设备凭证
	disconnect func()
}

// NewOnlineTerminals 建一份空的。
func NewOnlineTerminals() *OnlineTerminals {
	return &OnlineTerminals{terminals: map[int]onlineTerminal{}}
}

// add 记下一台连上的终端，返回去掉它用的编号。disconnect 断开这条连接。
func (o *OnlineTerminals) add(version, terminalID string, disconnect func()) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	o.terminals[o.next] = onlineTerminal{version: version, terminalID: terminalID, disconnect: disconnect}
	return o.next
}

func (o *OnlineTerminals) remove(id int) {
	o.mu.Lock()
	delete(o.terminals, id)
	o.mu.Unlock()
}

// Count 是在线几台、其中几台和服务端的版本不同（要升级）。
func (o *OnlineTerminals) Count() (online, needsUpgrade int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, terminal := range o.terminals {
		online++
		if buildinfo.Mismatch(buildinfo.Version, terminal.version) {
			needsUpgrade++
		}
	}
	return online, needsUpgrade
}

// Unidentified 是没带设备凭证、现在连着的终端（过渡期：都归管理员账号）：在线几台、几台要升级。
func (o *OnlineTerminals) Unidentified() (online, needsUpgrade int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, terminal := range o.terminals {
		if terminal.terminalID != "" {
			continue
		}
		online++
		if buildinfo.Mismatch(buildinfo.Version, terminal.version) {
			needsUpgrade++
		}
	}
	return online, needsUpgrade
}

// Status 是一台终端现在连没连着、连着时报的版本。
func (o *OnlineTerminals) Status(terminalID string) (online bool, version string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, terminal := range o.terminals {
		if terminal.terminalID == terminalID {
			return true, terminal.version
		}
	}
	return false, ""
}

// Disconnect 断开这台终端正连着的会话（在 App 里移除之后）。
func (o *OnlineTerminals) Disconnect(terminalID string) {
	o.mu.Lock()
	var disconnects []func()
	for _, terminal := range o.terminals {
		if terminal.terminalID == terminalID {
			disconnects = append(disconnects, terminal.disconnect)
		}
	}
	o.mu.Unlock()
	for _, disconnect := range disconnects {
		disconnect()
	}
}
