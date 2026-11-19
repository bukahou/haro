package braintest

import (
	"context"
	"sync"
)

// Camera 是假的终端摄像头（实现 brain.TerminalCamera）：可以设能不能拍、拍到什么、拍失败；记下拍了几次。
type Camera struct {
	mu        sync.Mutex
	available bool
	frame     []byte
	err       error
	captures  int
}

// NewCamera 创建能拍的假摄像头，每次拍到 frame。
func NewCamera(frame []byte) *Camera { return &Camera{available: true, frame: frame} }

// SetAvailable 设终端现在能不能拍（相当于它的工具清单里有没有「拍一帧」）。
func (c *Camera) SetAvailable(available bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.available = available
}

// FailWith 之后每次拍都返回 err。
func (c *Camera) FailWith(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// Captures 是一共拍了几次。
func (c *Camera) Captures() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.captures
}

func (c *Camera) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.available
}

func (c *Camera) Capture(context.Context) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captures++
	if c.err != nil {
		return nil, c.err
	}
	return c.frame, nil
}
