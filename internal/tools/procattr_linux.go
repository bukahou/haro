package tools

import "syscall"

// childProcAttr 让子进程在 haro 意外退出（包括被 SIGKILL）时也收到 SIGTERM，不留孤儿进程占着端口。
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
