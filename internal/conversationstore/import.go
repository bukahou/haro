package conversationstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// legacyRecord 是改存 SQLite 之前每人一个 JSON 文件的格式（只用来导入）：每一轮只有消息、没有时间。
type legacyRecord struct {
	Person        string                `json:"person"`
	PersonaID     string                `json:"persona_id"`
	History       [][]brain.ChatMessage `json:"history"`
	Summary       string                `json:"summary"`
	Unsummarized  [][]brain.ChatMessage `json:"unsummarized"`
	ToolCallCount int                   `json:"tool_call_count"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// importedSuffix 是导入后旧文件改成的后缀（留底，不删）。
const importedSuffix = ".imported"

// ImportJSONDir 把旧的对话文件（<目录>/<人>.json）导入库里，导入后改名留底；返回导入了几个人。
// 库里已经有这个人（之前导入过，之后又说过话）就不覆盖，只改名。目录不存在 = 没有要导入的。
// 出错就返回错误（服务端起不来、旧文件原样留着），不带着空历史启动。
func ImportJSONDir(ctx context.Context, store *SQLiteStore, dir string, logger *slog.Logger) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读旧的对话目录 %s: %w", dir, err)
	}
	imported := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return imported, err
		}
		var legacy legacyRecord
		if err := json.Unmarshal(data, &legacy); err != nil {
			return imported, fmt.Errorf("解析旧的对话文件 %s: %w", path, err)
		}
		if legacy.Person == "" {
			legacy.Person = strings.TrimSuffix(entry.Name(), ".json")
		}
		exists, err := store.has(ctx, legacy.Person)
		if err != nil {
			return imported, err
		}
		if exists {
			logger.Info("旧的对话文件：库里已经有这个人的对话，不再导入", "file", path)
		} else {
			if err := store.SaveConversation(legacy.record()); err != nil {
				return imported, fmt.Errorf("导入 %s: %w", path, err)
			}
			imported++
			logger.Info("导入旧的对话文件", "file", path, "person", legacy.Person, "turns", len(legacy.History))
		}
		if err := os.Rename(path, path+importedSuffix); err != nil {
			return imported, fmt.Errorf("导入后改名留底 %s: %w", path, err)
		}
	}
	return imported, nil
}

// record 换成现在的格式：旧记录没有每轮的时间，时间留空（显示「时间不详」，不编）。
func (l legacyRecord) record() brain.ConversationRecord {
	turns := func(groups [][]brain.ChatMessage) []brain.ConversationTurn {
		var out []brain.ConversationTurn
		for _, messages := range groups {
			out = append(out, brain.ConversationTurn{Messages: messages})
		}
		return out
	}
	return brain.ConversationRecord{Person: l.Person, PersonaID: l.PersonaID, History: turns(l.History), Summary: l.Summary,
		Unsummarized: turns(l.Unsummarized), ToolCallCount: l.ToolCallCount, UpdatedAt: l.UpdatedAt}
}
