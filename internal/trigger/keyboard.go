// Package trigger 是唤醒词之外的手动开关：在终端里按回车，休眠时立即唤醒，唤醒时立即休眠。
// 作为服务运行时没有键盘输入（stdin 为空），开关随之失效，只能用唤醒词。
package trigger

import (
	"bufio"
	"context"
	"io"
)

// KeyboardTrigger 从终端读回车。
type KeyboardTrigger struct {
	lines chan struct{}
}

// NewKeyboardTrigger 开一个后台 goroutine 持续读 input 的每一行。
func NewKeyboardTrigger(input io.Reader) *KeyboardTrigger {
	t := &KeyboardTrigger{lines: make(chan struct{})}
	go func() {
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			t.lines <- struct{}{}
		}
		close(t.lines)
	}()
	return t
}

// WaitForToggle 等用户按一次回车；输入结束时返回 io.EOF，ctx 取消时返回 ctx.Err()。
func (t *KeyboardTrigger) WaitForToggle(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case _, ok := <-t.lines:
		if !ok {
			return io.EOF
		}
		return nil
	}
}
