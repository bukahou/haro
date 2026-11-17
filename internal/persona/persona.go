// Package persona 读取性格档案（assets/personas/*.json），并把当前性格记在一个小文件里（重启后保持，P-3）。
// 性格的概念与切换逻辑在 brain 包；这里只负责「从哪读、记在哪」。
package persona

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bukahou/haro/internal/brain"
)

// file 是性格档案的文件格式。
type file struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Aliases        []string `json:"aliases"`
	DefaultEmotion string   `json:"default_emotion"` // tease / gentle …；空 = 用声音的 default 录音
	Prompt         string   `json:"prompt"`
}

// LoadCatalog 读取目录下全部 *.json 性格档案（按文件名排序），校验 ID、名字不重复、情绪合法。
func LoadCatalog(dir string) ([]brain.Persona, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%s 里没有性格档案（*.json）", dir)
	}
	sort.Strings(paths)
	seen := map[string]string{}
	personas := make([]brain.Persona, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var f file
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("解析性格档案 %s: %w", path, err)
		}
		if f.ID == "" || f.Name == "" || strings.TrimSpace(f.Prompt) == "" {
			return nil, fmt.Errorf("性格档案 %s：id、name、prompt 都必须填", path)
		}
		emotion, err := brain.ParseEmotion(f.DefaultEmotion)
		if err != nil {
			return nil, fmt.Errorf("性格档案 %s：%w", path, err)
		}
		for _, key := range []string{f.ID, f.Name} {
			if other, dup := seen[key]; dup {
				return nil, fmt.Errorf("性格档案 %s 与 %s 重名：%s", path, other, key)
			}
			seen[key] = path
		}
		personas = append(personas, brain.Persona{ID: f.ID, Name: f.Name, Aliases: f.Aliases,
			Prompt: strings.TrimSpace(f.Prompt), DefaultEmotion: emotion})
	}
	return personas, nil
}

// FileState 把当前性格的 ID 记在一个文本文件里（实现 brain.PersonaState）。
type FileState struct{ Path string }

// NewFileState 创建状态文件存储（目录不存在时，第一次保存会自动创建）。
func NewFileState(path string) *FileState { return &FileState{Path: path} }

// LoadPersona 读取记住的性格；文件不存在时返回 ""。
func (s *FileState) LoadPersona() (string, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(data)), err
}

// SavePersona 记住当前性格（先写临时文件再改名，写到一半断电也不会留下半个文件）。
func (s *FileState) SavePersona(id string) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}
