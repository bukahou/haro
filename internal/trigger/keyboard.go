// Package trigger 决定一轮对话什么时候开始、结束。
// 按键模式：按一次开始说，再按一次结束；免按键模式：按一次进入对话模式，再按一次退出。
// 以后换成树莓派 GPIO 按钮或唤醒词。
package trigger

import (
	"bufio"
	"context"
	"fmt"
	"io"
)

// KeyboardTrigger 从终端读回车。
type KeyboardTrigger struct {
	lines  chan struct{}
	output io.Writer
}

// NewKeyboardTrigger 开一个后台 goroutine 持续读 input 的每一行。
func NewKeyboardTrigger(input io.Reader, output io.Writer) *KeyboardTrigger {
	t := &KeyboardTrigger{lines: make(chan struct{}), output: output}
	go func() {
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			t.lines <- struct{}{}
		}
		close(t.lines)
	}()
	return t
}

// WaitForStart 等用户按回车开始说话。
func (t *KeyboardTrigger) WaitForStart(ctx context.Context) error {
	fmt.Fprint(t.output, "\n▶ 按回车开始说话 ")
	return t.waitForLine(ctx)
}

// WaitForStop 等用户再按一次回车结束。
func (t *KeyboardTrigger) WaitForStop(ctx context.Context) error {
	fmt.Fprint(t.output, "🎙 正在听……说完按回车 ")
	return t.waitForLine(ctx)
}

func (t *KeyboardTrigger) waitForLine(ctx context.Context) error {
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

// WaitForEnter 等用户按回车进入对话模式（v2 免按键）。
func (t *KeyboardTrigger) WaitForEnter(ctx context.Context) error {
	fmt.Fprint(t.output, "\n▶ 按回车进入对话模式 ")
	return t.waitForLine(ctx)
}

// WaitForLeave 等用户按回车退出对话模式。
func (t *KeyboardTrigger) WaitForLeave(ctx context.Context) error {
	fmt.Fprint(t.output, "🎙 对话模式：直接说话就行（按回车退出）\n")
	return t.waitForLine(ctx)
}

// WaitForToggle 等用户按一次回车（唤醒词模式下的手动开关，不打印提示）。
func (t *KeyboardTrigger) WaitForToggle(ctx context.Context) error {
	return t.waitForLine(ctx)
}
