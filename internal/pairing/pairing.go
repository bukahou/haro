// Package pairing 是终端一侧的登记（`haro -pair`，照 RFC 8628 设备授权的样子）：向服务端要一个配对码显示出来，
// 人在 App 的「添加终端」里输入它（或者请管理员在「新终端」里批准），终端定时问结果，批准了就把设备凭证存进文件；
// 之后连服务端时带上它（brainclient.DialOptions.Credential）。不配对的终端照旧能连（过渡期，只在局域网）。
package pairing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/buildinfo"
	"github.com/bukahou/haro/internal/endpoint"
	"github.com/bukahou/haro/internal/realtimeproto"
)

// Options 是一次登记的参数。
type Options struct {
	ServerURL      string // 服务端的地址（同 HARO_SERVER_URL）
	Name           string // 这台终端的名字（App 里显示、能改）
	CredentialFile string // 设备凭证存到哪里
	Out            io.Writer
	Client         *http.Client
}

// ErrRejected 是管理员拒绝了这台终端；ErrExpired 是配对码过期了（重新运行 haro -pair）。
var (
	ErrRejected = errors.New("管理员拒绝了这台终端")
	ErrExpired  = errors.New("配对码过期了：重新运行 haro -pair")
)

// Run 登记这台终端：显示配对码，等到批准后存下设备凭证。
func Run(ctx context.Context, opts Options) error {
	var started realtimeproto.RegistrationStarted
	if err := post(ctx, opts, realtimeproto.RegistrationPath, realtimeproto.RegistrationRequest{Name: opts.Name}, &started); err != nil {
		return fmt.Errorf("开始登记: %w", err)
	}
	fmt.Fprintf(opts.Out, "\n  配对码：%s（%d 分钟内有效）\n\n", started.Code, started.ExpiresIn/60)
	fmt.Fprintf(opts.Out, "在 App 的「添加终端」里输入这个配对码；或者请管理员在「新终端」里批准编号 %s 的这台。\n等结果中……\n",
		started.ShortID)
	interval := time.Duration(max(started.Interval, 1)) * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		var outcome realtimeproto.RegistrationOutcome
		err := post(ctx, opts, realtimeproto.RegistrationPollPath, realtimeproto.RegistrationPoll{PollToken: started.PollToken}, &outcome)
		var failure *httpFailure
		switch {
		case errors.As(err, &failure) && failure.code == realtimeproto.CodeRateLimited:
			interval *= 2 // 问得太勤：拉长间隔（RFC 8628 的 slow_down）
			continue
		case err != nil:
			return fmt.Errorf("问登记结果: %w", err)
		}
		switch outcome.Status {
		case realtimeproto.RegistrationPending:
			continue
		case realtimeproto.RegistrationApproved:
			if err := SaveCredential(opts.CredentialFile, outcome.Credential); err != nil {
				return err
			}
			fmt.Fprintf(opts.Out, "配对好了：设备凭证存在 %s。重启 haro（systemctl --user restart haro）后就带着它连服务端。\n",
				opts.CredentialFile)
			return nil
		case realtimeproto.RegistrationRejected:
			return ErrRejected
		case realtimeproto.RegistrationExpired:
			return ErrExpired
		}
		return fmt.Errorf("服务端回了不认识的结果 %q", outcome.Status)
	}
}

// httpFailure 是服务端回的出错（按错误码判断，不看文字）。
type httpFailure struct {
	status  int
	code    realtimeproto.ErrorCode
	message string
}

func (f *httpFailure) Error() string {
	return fmt.Sprintf("HTTP %d %s：%s", f.status, f.code, f.message)
}

func post(ctx context.Context, opts Options, path string, body, into any) error {
	address, err := endpoint.Join(opts.ServerURL, path, nil)
	if err != nil {
		return fmt.Errorf("服务端地址：%w", err)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", buildinfo.UserAgent())
	request.Header.Set(buildinfo.Header, buildinfo.Version)
	response, err := opts.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var problem realtimeproto.HTTPError
		_ = json.Unmarshal(payload, &problem)
		return &httpFailure{status: response.StatusCode, code: problem.Error.Code, message: problem.Error.Message}
	}
	return json.Unmarshal(payload, into)
}

// LoadCredential 读设备凭证；文件不存在 = 还没配对（空字符串，不算出错）。
func LoadCredential(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("读设备凭证 %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// SaveCredential 存设备凭证（只有自己能读；先写临时文件再改名，写一半不会留下坏文件）。
func SaveCredential(path, credential string) error {
	if credential == "" {
		return fmt.Errorf("服务端没给设备凭证")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("建目录 %s: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".device-credential-*")
	if err != nil {
		return fmt.Errorf("存设备凭证: %w", err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.WriteString(credential + "\n"); err != nil {
		_ = temp.Close()
		return fmt.Errorf("存设备凭证: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("存设备凭证: %w", err)
	}
	if err := os.Chmod(temp.Name(), 0o600); err != nil {
		return fmt.Errorf("存设备凭证: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("存设备凭证: %w", err)
	}
	return nil
}
