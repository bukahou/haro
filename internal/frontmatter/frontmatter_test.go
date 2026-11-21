package frontmatter

import (
	"errors"
	"testing"
)

// 拆法：配置和正文分开；Windows 换行、开头的 BOM 都认；没有配置、配置没结束分开报。
func TestSplit(t *testing.T) {
	cases := map[string]struct {
		data         string
		header, body string
		err          error
		failed       bool
	}{
		"配置加正文":      {data: "---\na: 1\n---\n正文\n", header: "a: 1\n", body: "正文\n"},
		"Windows 换行": {data: "---\r\na: 1\r\n---\r\n正文\r\n", header: "a: 1\n", body: "正文\n"},
		"开头有 BOM":    {data: "\uFEFF---\na: 1\n---\n正文", header: "a: 1\n", body: "正文"},
		"只有配置":       {data: "---\na: 1\n---", header: "a: 1\n"},
		"配置是空的":      {data: "---\n---\n正文", body: "正文"},
		"正文里也有分隔线":   {data: "---\na: 1\n---\n上\n---\n下", header: "a: 1\n", body: "上\n---\n下"},
		"没有配置":       {data: "只有正文\n", body: "只有正文\n", err: ErrMissing},
		"配置没结束":      {data: "---\na: 1\n", failed: true},
	}
	for label, c := range cases {
		header, body, err := Split([]byte(c.data))
		switch {
		case c.failed:
			if err == nil || errors.Is(err, ErrMissing) {
				t.Errorf("%s：应报配置没结束：%v", label, err)
			}
		case !errors.Is(err, c.err) || string(header) != c.header || string(body) != c.body:
			t.Errorf("%s：得到 %q / %q / %v，应为 %q / %q / %v", label, header, body, err, c.header, c.body, c.err)
		}
	}
}

// 解码：认识的字段解进来，不认识的报错，空的配置不报错。
func TestDecode(t *testing.T) {
	var out struct {
		Person []string `yaml:"person"`
	}
	if err := Decode([]byte("person: [主人, 太太]\n"), &out); err != nil || len(out.Person) != 2 {
		t.Errorf("列表：%+v %v", out, err)
	}
	if err := Decode([]byte("persons: 主人\n"), &out); err == nil {
		t.Error("不认识的字段应报错")
	}
	if err := Decode(nil, &out); err != nil {
		t.Errorf("空的配置不报错：%v", err)
	}
}
