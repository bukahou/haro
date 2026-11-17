//go:build !linux

package speech

import "syscall"

// childProcAttr 在非 Linux 平台没有 Pdeathsig，只能依赖 PiperServer.Stop 正常清理。
func childProcAttr() *syscall.SysProcAttr { return nil }
