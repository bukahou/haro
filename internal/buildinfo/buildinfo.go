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
	"encoding/json"
	"fmt"
	"io"
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

// Info 是一个程序的版本信息。-version -json 把它输出成一行 JSON，给机器读（安装脚本、CI 按字段判断，不解析给人看的那一行）。
type Info struct {
	Program string `json:"program"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Go      string `json:"go"`
}

// Report 是这个程序（program 是程序名，例如 haro）的版本信息。
func Report(program string) Info {
	return Info{Program: program, Version: Version, OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version()}
}

// Describe 是 -version 打印的一行（给人看），例如 haro v0.5.0-1a2b3c4 (linux/arm64, go1.27.1)。
func Describe(program string) string {
	info := Report(program)
	return fmt.Sprintf("%s %s (%s/%s, %s)", info.Program, info.Version, info.OS, info.Arch, info.Go)
}

// WriteVersion 写出 -version 的输出：asJSON 时是一行 JSON（Info），否则是给人看的那一行（Describe）。
func WriteVersion(w io.Writer, program string, asJSON bool) error {
	if !asJSON {
		_, err := fmt.Fprintln(w, Describe(program))
		return err
	}
	return json.NewEncoder(w).Encode(Report(program))
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
