// Package frontmatter 拆 Markdown 文件开头的 front matter（第一行 --- 到下一行 --- 之间的 YAML）并按结构体解码。
// 代理定义（internal/agents）和主人资料（internal/profile）共用这一份拆法：不认识的字段报错，写错了不悄悄忽略。
package frontmatter

import (
	"bytes"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"
)

// ErrMissing 表示文件开头没有 front matter（第一行不是 ---）。
var ErrMissing = errors.New("开头要是一行 ---，接着是配置")

const fence = "---\n"

// Split 把文件分成开头的配置（第一行 --- 到下一行 ---）和之后的正文。换行统一成 \n，开头的 BOM 去掉。
// 没有 front matter 时交回 ErrMissing（正文就是整个文件，由调用方决定算不算错）。
func Split(data []byte) (header, body []byte, err error) {
	data = bytes.TrimPrefix(data, []byte("\uFEFF"))
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte(fence)) {
		return nil, data, ErrMissing
	}
	rest := data[len(fence):]
	switch end := bytes.Index(rest, []byte("\n"+fence)); {
	case bytes.HasPrefix(rest, []byte(fence)): // 两行 --- 中间什么都没写
		return nil, rest[len(fence):], nil
	case bytes.Equal(rest, []byte("---")):
		return nil, nil, nil
	case end >= 0:
		return rest[:end+1], rest[end+1+len(fence):], nil
	case bytes.HasSuffix(rest, []byte("\n---")):
		return rest[:len(rest)-len("---")], nil, nil
	default:
		return nil, nil, errors.New("配置没有用一行 --- 结束")
	}
}

// Decode 按 YAML 把配置解进 out：不认识的字段报错；配置是空的时 out 保持原样。
func Decode(header []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(header))
	decoder.KnownFields(true)
	if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
