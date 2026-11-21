// Package docstest 是测试用的：读 docs/architecture.md 里由代码核对的部分（现在是「能触发哪些动作」的工具表），
// 让文档和代码对不上时测试失败。表的格式只在这里解析一处。
package docstest

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ToolTableHeading 是 architecture.md 里工具表所在小节的标题。
const ToolTableHeading = "### 工具"

// confirmMark 是工具表第二列「过确认闸」里的记号：有它 = 这个工具执行前要主人的声音 + 有时效的确认；空着 = 不用。
const confirmMark = "✓"

// toolName 匹配表格第一列里用反引号括起来的工具名。
var toolName = regexp.MustCompile("`([a-z_]+)`")

// ToolTable 读 path（architecture.md）里 ToolTableHeading 下面第一张表：工具名 → 要不要过确认闸。
// 第一列是一个或几个用反引号括起来的工具名，第二列是 confirmMark 或空着；格式不对、名字重复时测试失败。
func ToolTable(t testing.TB, path string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(data), "\n"+ToolTableHeading+"\n")
	if !found {
		t.Fatalf("%s 里没有小节「%s」", path, ToolTableHeading)
	}
	table := map[string]bool{}
	rows := 0
	for line := range strings.Lines(section) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			if rows > 0 { // 表结束
				break
			}
			continue
		}
		rows++
		if rows <= 2 { // 表头、分隔行
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			t.Fatalf("%s 工具表的这一行少了「过确认闸」那一列：%s", path, line)
		}
		var confirm bool
		switch mark := strings.TrimSpace(cells[1]); mark {
		case confirmMark:
			confirm = true
		case "":
		default:
			t.Fatalf("%s 工具表第二列只能是 %s 或空着，现在是 %q：%s", path, confirmMark, mark, line)
		}
		names := toolName.FindAllStringSubmatch(cells[0], -1)
		if len(names) == 0 {
			t.Fatalf("%s 工具表的这一行第一列没有工具名：%s", path, line)
		}
		for _, name := range names {
			if _, dup := table[name[1]]; dup {
				t.Fatalf("%s 工具表里 %s 写了两次", path, name[1])
			}
			table[name[1]] = confirm
		}
	}
	if len(table) == 0 {
		t.Fatalf("%s 的「%s」下面没读到工具表", path, ToolTableHeading)
	}
	return table
}
