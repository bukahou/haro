// Package buildinfo 是程序的版本号（v6 CI/CD）。
//
// 版本号 = 仓库根目录 VERSION 文件 + 提交号，例如 v0.5.0-1a2b3c4；
// 和 GitHub Release 的标签、服务端镜像的标签是同一个字符串（看到一个就能找到另外两个）。
// 构建时用 -ldflags 写入（GitHub Actions、make build、Dockerfile 都这么做）：
//
//	go build -ldflags "-X github.com/bukahou/haro/internal/buildinfo.Version=v0.5.0-1a2b3c4" ./cmd/haro
//
// 直接 go build / go run（没写入）时是 DevVersion。
package buildinfo

import (
	"fmt"
	"runtime"
)

// DevVersion 是没写入版本号的本地构建：不和任何版本比较。
const DevVersion = "dev"

// Version 是这个程序的版本号。
var Version = DevVersion

// Header 是终端和服务端握手时互报版本用的 HTTP 头（v6 T6）：终端写在请求里，服务端写在响应里。
const Header = "X-Haro-Version"

// UserAgent 是终端连服务端时用的 User-Agent，例如 haro/v0.5.0-1a2b3c4。
func UserAgent() string { return "haro/" + Version }

// Describe 是 -version 打印的一行，例如 haro v0.5.0-1a2b3c4 (linux/arm64, go1.27.1)。
func Describe(program string) string {
	return fmt.Sprintf("%s %s (%s/%s, %s)", program, Version, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// Mismatch 判断两边的版本是否不同（值得在日志里提醒升级）。
// 本地构建（dev）不和别人比；对方没报版本（v0.5.0 以前的程序）算不同。
func Mismatch(ours, theirs string) bool {
	if ours == DevVersion || theirs == DevVersion {
		return false
	}
	return ours != theirs
}

// Reported 把对方报的版本变成日志里能看的样子：没报时说明是旧程序。
func Reported(version string) string {
	if version == "" {
		return "未报（v0.5.0 以前的程序）"
	}
	return version
}
