package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/notes"
)

// NoteTools 返回记事本的四个内置工具。
// v5 裁定：自研工具直接内置到服务端（记事本只是一个数据库连接，没必要单独成程序），MCP 只用于第三方服务。
// 工具名、说明、参数、结果格式与原 haro-notes（MCP 服务）完全一致，模型看到的东西不变。
func NoteTools(store notes.Repository, logger *slog.Logger) []Builtin {
	logger = logger.With("tool", "notes")
	return []Builtin{
		{Spec: brain.ToolSpec{Name: "save_note", Description: "把主人要求记下的事情保存到记事本。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"要记的内容（主人的原话）"}},"required":["content"]}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				var in struct {
					Content string `json:"content"`
				}
				if err := decodeArguments(arguments, &in); err != nil {
					return "", err
				}
				note, err := store.Save(ctx, in.Content)
				logger.Info("记一条", "id", note.ID, "err", err)
				if err != nil {
					return "", err
				}
				return marshalResult(note)
			}},
		{Spec: brain.ToolSpec{Name: "search_notes", Description: "在记事本里查找以前记过的事情（按内容相关度，最多 5 条）。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"关键词或问题，例如「车停在哪」"}},"required":["query"]}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				var in struct {
					Query string `json:"query"`
				}
				if err := decodeArguments(arguments, &in); err != nil {
					return "", err
				}
				found, err := store.Search(ctx, in.Query, 5)
				if err != nil {
					return "", err
				}
				return marshalNotes(found)
			}},
		{Spec: brain.ToolSpec{Name: "list_recent_notes", Description: "列出记事本里最近记的几条。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","description":"条数，默认 5，最多 20"}}}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				var in struct {
					Limit int `json:"limit"`
				}
				if err := decodeArguments(arguments, &in); err != nil {
					return "", err
				}
				if in.Limit <= 0 || in.Limit > 20 {
					in.Limit = 5
				}
				recent, err := store.Recent(ctx, in.Limit)
				if err != nil {
					return "", err
				}
				return marshalNotes(recent)
			}},
		// 按内容描述删除，而不是要求模型先查出 id：4B 小模型不愿意分两步做，实测会直接谎称「已删」（2026-11-15）
		{Spec: brain.ToolSpec{Name: "delete_note", RequiresConfirmation: true, // 删除要主人口头确认（v2.5 T3）
			Description: "删除记事本里的记录（可恢复的软删除），一次可以删多条：queries 里每个描述删掉和它最匹配的一条。" +
				"删除前先用 search_notes 找到确切的记录，再用记录原文调用。主人要求删除时必须调用它。" +
				"结果里 not_found 是没找到的描述，要如实告诉主人。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"queries":{"type":"array","items":{"type":"string"},"description":"要删的每一条的描述，一条一个；最好用 search_notes 查到的原文，例如「明天下午去取快递」"}},"required":["queries"]}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				var in struct {
					Queries []string `json:"queries"`
				}
				if err := decodeArguments(arguments, &in); err != nil {
					return "", err
				}
				return deleteNotes(ctx, store, in.Queries, logger)
			}},
	}
}

// deleteNotes 按描述逐条删除：找到的删掉、找不到的写进 not_found（v3 D-1，不整体报错）。
func deleteNotes(ctx context.Context, store notes.Repository, queries []string, logger *slog.Logger) (string, error) {
	if len(queries) == 0 {
		return "", errors.New("queries 不能为空：要说清楚删哪一条")
	}
	outcome := struct {
		Deleted  []string `json:"deleted"`
		NotFound []string `json:"not_found"`
	}{Deleted: []string{}, NotFound: []string{}}
	for _, query := range queries {
		query = strings.TrimSpace(query)
		if query == "" {
			continue
		}
		matches, err := store.Search(ctx, query, 1)
		if err != nil {
			return "", err
		}
		if len(matches) == 0 {
			outcome.NotFound = append(outcome.NotFound, query)
			continue
		}
		note, err := store.Delete(ctx, matches[0].ID)
		logger.Info("删除", "id", matches[0].ID, "query", query, "err", err)
		if errors.Is(err, notes.ErrNotFound) { // 同一条被两个描述命中：第二次算没找到
			outcome.NotFound = append(outcome.NotFound, query)
			continue
		}
		if err != nil {
			return "", err
		}
		outcome.Deleted = append(outcome.Deleted, note.Content)
	}
	return marshalResult(outcome)
}

func marshalNotes(list []notes.Note) (string, error) {
	if list == nil {
		list = []notes.Note{}
	}
	return marshalResult(map[string]any{"notes": list})
}

// decodeArguments 把模型给的参数（JSON 对象）解到结构体里。
func decodeArguments(arguments map[string]any, target any) error {
	if arguments == nil {
		return nil
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("参数格式不对: %w", err)
	}
	return nil
}
