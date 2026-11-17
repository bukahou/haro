// Package conversationstore 存对话（v4 D1，实现 brain.ConversationStore）：每人一个 JSON 文件，<目录>/<人>.json。
//
// 对话里是家里人说过的话：文件权限 0600（和声纹一样），先写临时文件再改名（写到一半断电也不会坏）。
// 以后要多副本大脑时换成 Redis 等实现，大脑其他代码不用改。
package conversationstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/bukahou/haro/internal/brain"
)

// FileStore 把对话存在一个目录里。
type FileStore struct{ dir string }

// NewFileStore 创建文件存储（目录不存在时第一次保存会建）。
func NewFileStore(dir string) *FileStore { return &FileStore{dir: dir} }

// validPerson：名字直接做文件名（和声纹登记的名字同一规则，另外允许 brain.SharedPerson 的下划线开头）。
var validPerson = regexp.MustCompile(`^[\p{L}\p{N}_-]{1,32}$`)

func (f *FileStore) path(person string) (string, error) {
	if !validPerson.MatchString(person) {
		return "", fmt.Errorf("名字 %q 不能做对话文件名", person)
	}
	return filepath.Join(f.dir, person+".json"), nil
}

// LoadConversation 读一个人的对话；没存过时 ok=false。
func (f *FileStore) LoadConversation(person string) (brain.ConversationRecord, bool, error) {
	path, err := f.path(person)
	if err != nil {
		return brain.ConversationRecord{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return brain.ConversationRecord{}, false, nil
	}
	if err != nil {
		return brain.ConversationRecord{}, false, err
	}
	var record brain.ConversationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return brain.ConversationRecord{}, false, fmt.Errorf("解析对话 %s: %w", path, err)
	}
	return record, true, nil
}

// SaveConversation 写一个人的对话。
func (f *FileStore) SaveConversation(record brain.ConversationRecord) error {
	path, err := f.path(record.Person)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
