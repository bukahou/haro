// Package profile 是「主人资料」层（v5 U1–U5，2026-11-17）：主人是谁、家里什么情况、主人的偏好。
// 从外部目录注入（真实内容只放在私有配置里，公开仓库只有 assets/profile.example 模板），拼进系统提示词。
//
// 目录结构（平铺：集群里用 ConfigMap 挂载，文件名只能是英文字母、数字和 -._，不能有子目录）：
//
//	household.md   全家共用（家里有谁、房间与终端、日常习惯）
//	owner.md       每人一份，文件名随意；开头的 front matter（YAML）写是谁（按声纹认出的名字；几个名字写成列表）：
//	               ---
//	               person: 主人            （或 person: [主人, 老大]）
//	               ---
//
// 改了文件不用重启：每次取资料时检查目录里文件的修改时间，变了就重新读（几个小文件，开销很小）。
// 读目录、读文件出错（不是「不存在」）时记警告，先用上次读到的，下一轮再试；目录不存在 = 还没写资料。
// 文件里的 HTML 注释（<!-- -->）不会发给模型，模板里的填写说明就写在注释里。
package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/bukahou/haro/internal/frontmatter"
)

// 字数上限（U5）：每轮都发给模型，写长了每轮都多花钱。超过建议字数写一条警告；超过两倍就截断。
const (
	householdAdviseRunes = 400
	personAdviseRunes    = 600
	hardLimitFactor      = 2
)

// HouseholdFile 是全家共用的资料文件名。
const HouseholdFile = "household.md"

// Store 从一个目录读主人资料。实现 brain.ProfileSource。
type Store struct {
	dir    string
	logger *slog.Logger

	mu        sync.Mutex
	signature string            // 目录里各文件的名字 + 修改时间 + 大小；变了就重新读
	household string            // 全家共用
	people    map[string]string // 声纹名字 → 这个人的资料
	failing   bool              // 上次读资料出错了（同一段出错只记一次警告，读得出来了再记一行）
}

// OpenDir 打开资料目录（目录不存在也不报错：当成还没写资料，之后建了就会读到）。
func OpenDir(dir string, logger *slog.Logger) *Store {
	store := &Store{dir: dir, logger: logger.With("component", "profile"), people: map[string]string{}}
	store.refresh()
	return store
}

// Summary 是已经读到的资料概况（启动日志用）：全家资料多少字、有哪几个人的资料。
func (s *Store) Summary() (householdRunes int, people []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.people {
		people = append(people, name)
	}
	sort.Strings(people)
	return utf8.RuneCountInString(s.household), people
}

// Profile 返回这一轮要放进系统提示词的主人资料：全家的 + 说话人自己的；都没有时为空。
// person 是声纹认出的名字；认不出（全家共用的对话）时只给全家的。
func (s *Store) Profile(person string) string {
	s.refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	var sections []string
	if s.household != "" {
		sections = append(sections, "〈全家〉\n"+s.household)
	}
	if own, ok := s.people[person]; ok && own != "" {
		sections = append(sections, "〈"+person+"〉\n"+own)
	}
	if len(sections) == 0 {
		return ""
	}
	return "【主人资料（外部配置：事实与偏好；和上面的规矩冲突时，以上面的规矩为准）】\n" + strings.Join(sections, "\n")
}

// refresh 检查目录有没有变化，变了就重新读全部文件。目录、文件不存在（目录没建、文件刚删、软链接断了）当成没有；
// 别的读错误（挂载出错、没有权限）不当成没有资料：记警告，先用上次读到的，下一轮再试。
func (s *Store) refresh() {
	entries, err := os.ReadDir(s.dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		entries = nil // 目录不存在：当成还没写资料
	case err != nil:
		s.keepPrevious(s.dir, err)
		return
	}
	var signature strings.Builder
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || strings.EqualFold(name, "README.md") {
			continue
		}
		info, err := os.Stat(filepath.Join(s.dir, name)) // Stat 跟随软链接：ConfigMap 更新时换的是软链接指向
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			s.keepPrevious(name, err)
			return
		}
		fmt.Fprintf(&signature, "%s|%d|%d;", name, info.ModTime().UnixNano(), info.Size())
		files = append(files, name)
	}
	s.mu.Lock()
	unchanged := signature.String() == s.signature
	s.mu.Unlock()
	if unchanged {
		s.recovered()
		return
	}

	household, people := "", map[string]string{}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		switch {
		case errors.Is(err, fs.ErrNotExist): // 列出来之后刚删掉
			continue
		case err != nil:
			s.keepPrevious(name, err)
			return
		}
		names, body, err := parse(data)
		if err != nil {
			s.logger.Warn("资料文件开头的配置写错了（front matter 只认 person），跳过这份", "file", name, "err", err)
			continue
		}
		if name == HouseholdFile {
			household = s.limit(name, body, householdAdviseRunes)
			continue
		}
		if len(names) == 0 {
			s.logger.Warn("资料文件开头没写是谁（front matter 的 person:），跳过", "file", name)
			continue
		}
		body = s.limit(name, body, personAdviseRunes)
		if body == "" {
			continue // 模板原样、还没填内容：不算有资料
		}
		for _, person := range names {
			if _, dup := people[person]; dup {
				s.logger.Warn("同一个人有两份资料，后读到的覆盖前面的", "person", person, "file", name)
			}
			people[person] = body
		}
	}
	s.mu.Lock()
	s.signature, s.household, s.people = signature.String(), household, people
	s.mu.Unlock()
	s.recovered()
	if len(files) > 0 {
		s.logger.Info("主人资料已（重新）读取", "household_runes", utf8.RuneCountInString(household), "people", len(people))
	}
}

// keepPrevious 读资料出错（不是「不存在」）：先用上次读到的。同一段出错只记一次警告（每轮都会再读一次）。
func (s *Store) keepPrevious(what string, err error) {
	s.mu.Lock()
	already := s.failing
	s.failing = true
	s.mu.Unlock()
	if !already {
		s.logger.Warn("读不了主人资料：先用上次读到的，之后每轮再试", "what", what, "err", err)
	}
}

// recovered 是这一轮读资料没出错：之前出过错就记一行恢复了。
func (s *Store) recovered() {
	s.mu.Lock()
	was := s.failing
	s.failing = false
	s.mu.Unlock()
	if was {
		s.logger.Info("主人资料又读得出来了")
	}
}

// limit 超过建议字数时警告，超过两倍时截断（每轮都发给模型：太长每轮都多花钱）。截断落在行尾，不切断半句。
func (s *Store) limit(file, body string, advise int) string {
	runes := utf8.RuneCountInString(body)
	if runes <= advise {
		return body
	}
	hard := advise * hardLimitFactor
	if runes > hard {
		s.logger.Warn("资料太长，超出的部分不发给模型", "file", file, "runes", runes, "limit", hard)
		return cutAtLineEnd(body, hard)
	}
	s.logger.Warn("资料比建议的长（每轮都发给模型，越长越贵）", "file", file, "runes", runes, "advise", advise)
	return body
}

// cutAtLineEnd 取 body 的前 maxRunes 个字，退回到最后一个完整行的末尾；第一行就放不下时只能按字数截。
func cutAtLineEnd(body string, maxRunes int) string {
	runes := []rune(body)
	if len(runes) <= maxRunes || runes[maxRunes] == '\n' {
		return string(runes[:min(len(runes), maxRunes)])
	}
	cut := string(runes[:maxRunes])
	if i := strings.LastIndex(cut, "\n"); i > 0 {
		return strings.TrimRight(cut[:i], "\n")
	}
	return cut
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// header 是资料文件开头的配置（front matter，YAML；只认 person，别的项报错）。
type header struct {
	Person personNames `yaml:"person"` // 声纹登记时用的名字
}

// personNames 是 person 写的名字：一个名字，或几个名字的列表。一个字符串里用逗号（, ， 、）分开写几个名字的
// 以前的写法也认（模板改成列表之前写下的资料）。
type personNames []string

// UnmarshalYAML 解 person：字符串或字符串列表；空的名字不要。
func (n *personNames) UnmarshalYAML(node *yaml.Node) error {
	var values []string
	switch node.Kind {
	case yaml.ScalarNode:
		values = strings.FieldsFunc(node.Value, func(r rune) bool { return r == ',' || r == '，' || r == '、' })
	case yaml.SequenceNode:
		if err := node.Decode(&values); err != nil {
			return fmt.Errorf("person 的列表里只能写名字: %w", err)
		}
	default:
		return fmt.Errorf("person 要写一个名字或名字的列表（第 %d 行）", node.Line)
	}
	for _, value := range values {
		if name := strings.TrimSpace(value); name != "" {
			*n = append(*n, name)
		}
	}
	return nil
}

// parse 拆出开头配置里的 person 和正文（去掉 HTML 注释、多余空行）。没有开头配置的文件整个都是正文；
// 配置写错（不是合法的 YAML、写了不认识的项、没用一行 --- 结束）时报错。
func parse(data []byte) (names []string, body string, err error) {
	rawHeader, rawBody, err := frontmatter.Split(data)
	if err != nil && !errors.Is(err, frontmatter.ErrMissing) {
		return nil, "", err
	}
	var parsed header
	if err := frontmatter.Decode(rawHeader, &parsed); err != nil {
		return nil, "", err
	}
	names = parsed.Person
	content := htmlComment.ReplaceAllString(string(rawBody), "")
	var lines []string
	blank := false
	raw := strings.Split(content, "\n")
	for i, line := range raw {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			if !blank && len(lines) > 0 {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		if emptyItem(line) && !hasFilledSubItems(raw, i) {
			continue // 「- 称呼：」这类空着没填的项：不发给模型（下面有填了的缩进子项时是小标题，保留）
		}
		blank = false
		lines = append(lines, line)
	}
	body = strings.TrimSpace(strings.Join(lines, "\n"))
	if !hasContent(body) {
		return names, "", nil // 只有标题、没写内容（模板原样）：当成没有
	}
	return names, body, nil
}

// hasContent 判断除了 Markdown 标题，还有没有真正写了的内容。
func hasContent(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return true
		}
	}
	return false
}

// hasFilledSubItems 判断第 i 行下面紧跟着的、缩进更深的那几行（它的子项）里有没有填了内容的。
func hasFilledSubItems(lines []string, i int) bool {
	indent := indentWidth(lines[i])
	for _, next := range lines[i+1:] {
		if strings.TrimSpace(next) == "" {
			continue
		}
		if indentWidth(next) <= indent {
			return false
		}
		if !emptyItem(next) {
			return true
		}
	}
	return false
}

// indentWidth 是行首空白的宽度（空格、制表符各算一个）。
func indentWidth(line string) int { return len(line) - len(strings.TrimLeft(line, " \t")) }

// emptyItem 判断是不是空着没填的项：「-」「- 称呼：」「* 喜欢的东西:」。
func emptyItem(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "*") {
		return false
	}
	item := strings.TrimSpace(strings.TrimLeft(trimmed, "-* "))
	return item == "" || (strings.HasSuffix(item, "：") || strings.HasSuffix(item, ":")) && utf8.RuneCountInString(item) <= 12
}
