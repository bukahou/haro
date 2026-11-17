// Package profile 是「主人资料」层（v5 U1–U5，2026-11-17）：主人是谁、家里什么情况、主人的偏好。
// 从外部目录注入（真实内容只放在私有配置里，公开仓库只有 assets/profile.example 模板），拼进系统提示词。
//
// 目录结构（平铺：集群里用 ConfigMap 挂载，文件名只能是英文字母、数字和 -._，不能有子目录）：
//
//	household.md   全家共用（家里有谁、房间与终端、日常习惯）
//	owner.md       每人一份，文件名随意；开头的 front matter 写是谁（按声纹认出的名字，可以写几个，用逗号分隔）：
//	               ---
//	               person: 主人
//	               ---
//
// 改了文件不用重启：每次取资料时检查目录里文件的修改时间，变了就重新读（几个小文件，开销很小）。
// 文件里的 HTML 注释（<!-- -->）不会发给模型，模板里的填写说明就写在注释里。
package profile

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
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

// refresh 检查目录有没有变化，变了就重新读全部文件。
func (s *Store) refresh() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		entries = nil // 目录不存在：当成没有资料
	}
	var signature strings.Builder
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") || strings.EqualFold(name, "README.md") {
			continue
		}
		info, err := os.Stat(filepath.Join(s.dir, name)) // Stat 跟随软链接：ConfigMap 更新时换的是软链接指向
		if err != nil {
			continue
		}
		fmt.Fprintf(&signature, "%s|%d|%d;", name, info.ModTime().UnixNano(), info.Size())
		files = append(files, name)
	}
	s.mu.Lock()
	unchanged := signature.String() == s.signature
	s.mu.Unlock()
	if unchanged {
		return
	}

	household, people := "", map[string]string{}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			s.logger.Warn("读不了资料文件", "file", name, "err", err)
			continue
		}
		names, body := parse(string(data))
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
	if len(files) > 0 {
		s.logger.Info("主人资料已（重新）读取", "household_runes", utf8.RuneCountInString(household), "people", len(people))
	}
}

// limit 超过建议字数时警告，超过两倍时截断（每轮都发给模型：太长每轮都多花钱）。
func (s *Store) limit(file, body string, advise int) string {
	runes := utf8.RuneCountInString(body)
	if runes <= advise {
		return body
	}
	hard := advise * hardLimitFactor
	if runes > hard {
		s.logger.Warn("资料太长，超出的部分不发给模型", "file", file, "runes", runes, "limit", hard)
		return string([]rune(body)[:hard])
	}
	s.logger.Warn("资料比建议的长（每轮都发给模型，越长越贵）", "file", file, "runes", runes, "advise", advise)
	return body
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// parse 拆出 front matter 里的 person（可以几个名字，逗号分隔）和正文（去掉 HTML 注释、多余空行）。
func parse(content string) (names []string, body string) {
	content = strings.TrimPrefix(content, "\uFEFF")
	if strings.HasPrefix(content, "---\n") || strings.HasPrefix(content, "---\r\n") {
		rest := content[strings.Index(content, "\n")+1:]
		if end := strings.Index(rest, "\n---"); end >= 0 {
			header := rest[:end]
			content = rest[end+len("\n---"):]
			if i := strings.Index(content, "\n"); i >= 0 {
				content = content[i+1:]
			} else {
				content = ""
			}
			scanner := bufio.NewScanner(strings.NewReader(header))
			for scanner.Scan() {
				key, value, ok := strings.Cut(scanner.Text(), ":")
				if !ok || strings.TrimSpace(key) != "person" {
					continue
				}
				for _, name := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '，' || r == '、' }) {
					if name = strings.Trim(strings.TrimSpace(name), `"'`); name != "" {
						names = append(names, name)
					}
				}
			}
		}
	}
	content = htmlComment.ReplaceAllString(content, "")
	var lines []string
	blank := false
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			if !blank && len(lines) > 0 {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		if emptyItem(line) {
			continue // 「- 称呼：」这类空着没填的项：不发给模型
		}
		blank = false
		lines = append(lines, line)
	}
	body = strings.TrimSpace(strings.Join(lines, "\n"))
	if !hasContent(body) {
		return names, "" // 只有标题、没写内容（模板原样）：当成没有
	}
	return names, body
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

// emptyItem 判断是不是空着没填的项：「-」「- 称呼：」「* 喜欢的东西:」。
func emptyItem(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "*") {
		return false
	}
	item := strings.TrimSpace(strings.TrimLeft(trimmed, "-* "))
	return item == "" || (strings.HasSuffix(item, "：") || strings.HasSuffix(item, ":")) && utf8.RuneCountInString(item) <= 12
}
