package brainserver

import (
	"sync"

	"github.com/bukahou/haro/internal/buildinfo"
)

// OnlineTerminals 记着现在连着的 haro 终端（连上时加、断开时去掉）：App 的概览要看在线几台、几台要升级。
// 只是在线的这一刻；终端的身份、名字、属于哪个账号在终端配对之后才有（第 15 段 15c）。
type OnlineTerminals struct {
	mu        sync.Mutex
	next      int
	terminals map[int]string // 连接编号 → 终端报的版本
}

// NewOnlineTerminals 建一份空的。
func NewOnlineTerminals() *OnlineTerminals { return &OnlineTerminals{terminals: map[int]string{}} }

// add 记下一台连上的终端，返回去掉它用的编号。
func (o *OnlineTerminals) add(version string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	o.terminals[o.next] = version
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
	for _, version := range o.terminals {
		online++
		if buildinfo.Mismatch(buildinfo.Version, version) {
			needsUpgrade++
		}
	}
	return online, needsUpgrade
}
