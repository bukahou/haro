// Package endpoint 由配置里的服务地址拼出请求地址（HTTP 和 WebSocket）：按结构解析，不做字符串拼接。
//
// 地址里原有的路径、查询参数都保留（例如付费接口要的 ?apikey=…）；代码里写死的路径原样接上；
// 运行时来的值（下载的地区表里的代码、声纹的名字……）逐段按路径段转义，/ 也转义，不能是空、. 或 ..。
package endpoint

import (
	"fmt"
	"net/url"
	"strings"
)

// Join 由服务地址 base（http(s)://主机[:端口][/路径][?查询]）拼出请求地址：base 原有的路径后面接上 path
// （代码里写死的路径，可以带 /，空 = 不接），再逐段接上 segments（每段按路径段转义）；query 合并进 base 原有的查询参数。
func Join(base, path string, query url.Values, segments ...string) (string, error) {
	address, err := parse(base, "http", "https")
	if err != nil {
		return "", err
	}
	if err := extend(address, path, query, segments); err != nil {
		return "", err
	}
	return address.String(), nil
}

// WebSocket 和 Join 一样拼地址，另把 scheme 换成 WebSocket 的：http → ws、https → wss，已经是 ws(s) 的原样用。
func WebSocket(base, path string, query url.Values, segments ...string) (string, error) {
	address, err := parse(base, "http", "https", "ws", "wss")
	if err != nil {
		return "", err
	}
	switch address.Scheme {
	case "http":
		address.Scheme = "ws"
	case "https":
		address.Scheme = "wss"
	}
	if err := extend(address, path, query, segments); err != nil {
		return "", err
	}
	return address.String(), nil
}

// parse 解析服务地址：scheme 要是 schemes 之一（url.Parse 已转成小写），要有主机名。
func parse(base string, schemes ...string) (*url.URL, error) {
	address, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("服务地址 %q：%w", base, err)
	}
	known := false
	for _, scheme := range schemes {
		known = known || address.Scheme == scheme
	}
	if !known {
		return nil, fmt.Errorf("服务地址 %q 要以 %s:// 开头", base, strings.Join(schemes, ":// 或 "))
	}
	if address.Host == "" {
		return nil, fmt.Errorf("服务地址 %q 里没有主机名", base)
	}
	return address, nil
}

// extend 在 address 原有的路径后面接上 path 和 segments，把 query 合并进原有的查询参数。
func extend(address *url.URL, path string, query url.Values, segments []string) error {
	parts := []string{strings.TrimSuffix(address.EscapedPath(), "/")}
	if fixed := strings.Trim(path, "/"); fixed != "" {
		parts = append(parts, (&url.URL{Path: fixed}).EscapedPath())
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("路径段 %q 不能用（不能是空、. 或 ..）", segment)
		}
		parts = append(parts, url.PathEscape(segment))
	}
	if len(parts) > 1 {
		escaped := strings.Join(parts, "/")
		unescaped, err := url.PathUnescape(escaped)
		if err != nil {
			return fmt.Errorf("路径 %q：%w", escaped, err)
		}
		address.Path, address.RawPath = unescaped, escaped
	}
	if len(query) > 0 {
		merged := address.Query()
		for key, values := range query {
			merged[key] = append(merged[key], values...)
		}
		address.RawQuery = merged.Encode()
	}
	return nil
}
