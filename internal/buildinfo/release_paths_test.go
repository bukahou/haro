package buildinfo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// 发版流程（.github/workflows/release.yml）的 paths-ignore 决定哪些合并不发版：编进程序的文件（go:embed）改了必须发版，
// 所以它们一个都不能被 paths-ignore 匹配到。编进程序的文件按 go list 的 EmbedFiles 取，不按文件名猜。
func TestReleaseIgnoresNoEmbeddedFile(t *testing.T) {
	root := filepath.Join("..", "..")
	patterns := releasePathsIgnore(t, filepath.Join(root, ".github", "workflows", "release.yml"))
	embedded := embeddedFiles(t, root)
	// 阳性对照：确实扫到了编进程序的提示词（扫不到时下面的检查等于没做）
	const prompt = "internal/agents/builtin/front.md"
	found := false
	for _, file := range embedded {
		found = found || file == prompt
	}
	if !found {
		t.Fatalf("go list 列出的编进程序的文件里没有 %s：检查装置没生效（列出了 %d 个）", prompt, len(embedded))
	}
	for _, pattern := range patterns {
		matcher, err := githubPathPattern(pattern)
		if err != nil {
			t.Fatalf("release.yml 的 paths-ignore %q：%v", pattern, err)
		}
		for _, file := range embedded {
			if matcher.MatchString(file) {
				t.Errorf("release.yml 的 paths-ignore %q 匹配到编进程序的 %s：只改它的合并不会发版", pattern, file)
			}
		}
	}
}

// 路径规则的换算照 GitHub 的写法：* 不跨目录，** 跨目录，**/ 也可以是零层目录。
func TestGitHubPathPattern(t *testing.T) {
	for _, c := range []struct {
		pattern, path string
		match         bool
	}{
		{"**/*.md", "internal/agents/builtin/front.md", true},
		{"**/*.md", "README.md", true},
		{"*.md", "README.md", true},
		{"*.md", "internal/agents/builtin/front.md", false},
		{"docs/**", "docs/architecture.md", true},
		{"docs/**", "deploy/docs.md", false},
		{"LICENSE", "LICENSE", true},
		{"LICENSE", "deploy/server/MODEL-LICENSES.txt", false},
	} {
		matcher, err := githubPathPattern(c.pattern)
		if err != nil {
			t.Fatalf("%q：%v", c.pattern, err)
		}
		if got := matcher.MatchString(c.path); got != c.match {
			t.Errorf("%q 对 %s：匹配 = %v，应为 %v", c.pattern, c.path, got, c.match)
		}
	}
	for _, unsupported := range []string{"!docs/**", "src/?.go", "a+", "[ab].md"} {
		if _, err := githubPathPattern(unsupported); err == nil {
			t.Errorf("%q 用了没有换算的写法，应报错（不能悄悄当成普通字符）", unsupported)
		}
	}
}

// releasePathsIgnore 读发版流程里 on.push.paths-ignore 的每一条。
func releasePathsIgnore(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On struct {
			Push struct {
				PathsIgnore []string `yaml:"paths-ignore"`
			} `yaml:"push"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("解析 %s：%v", path, err)
	}
	if len(workflow.On.Push.PathsIgnore) == 0 {
		t.Fatalf("%s 里没读到 on.push.paths-ignore", path)
	}
	return workflow.On.Push.PathsIgnore
}

// embeddedFiles 是 go:embed 编进程序的文件（相对仓库根目录、用 / 分隔）。
func embeddedFiles(t *testing.T, root string) []string {
	t.Helper()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "list", "-json=Dir,EmbedFiles", "./...")
	command.Dir = absRoot
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list：%v\n%s", err, stderr.String())
	}
	var files []string
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg struct {
			Dir        string
			EmbedFiles []string
		}
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("解析 go list 的输出：%v", err)
		}
		for _, name := range pkg.EmbedFiles {
			relative, err := filepath.Rel(absRoot, filepath.Join(pkg.Dir, name))
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, filepath.ToSlash(relative))
		}
	}
	return files
}

// githubPathPattern 把 GitHub Actions 路径过滤的一条规则换成正则：* 匹配除 / 以外的任意字符，** 匹配任意字符，
// 开头或中间的 **/ 也可以是零层目录。只换算这几种写法；?、+、[]、开头的 !（在 GitHub 里另有含义）一律报错。
func githubPathPattern(pattern string) (*regexp.Regexp, error) {
	if strings.HasPrefix(pattern, "!") {
		return nil, fmt.Errorf("不支持开头的 !")
	}
	var expression strings.Builder
	expression.WriteString("^")
	for rest := pattern; rest != ""; {
		switch {
		case strings.HasPrefix(rest, "**/"):
			expression.WriteString("(?:.*/)?")
			rest = rest[len("**/"):]
		case strings.HasPrefix(rest, "**"):
			expression.WriteString(".*")
			rest = rest[len("**"):]
		case rest[0] == '*':
			expression.WriteString("[^/]*")
			rest = rest[1:]
		case strings.ContainsRune("?+[]", rune(rest[0])):
			return nil, fmt.Errorf("不支持 %q", rest[0])
		default:
			expression.WriteString(regexp.QuoteMeta(rest[:1]))
			rest = rest[1:]
		}
	}
	expression.WriteString("$")
	return regexp.Compile(expression.String())
}
