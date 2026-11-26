package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/notes"
)

// deletedListLimit 是「最近删掉的」最多列几条。
const deletedListLimit = 20

// NoteTools 是记事本的工具：记、找（把记录连编号交给模型挑）、按编号删、列出最近删掉的、按编号恢复。
// 找哪一条由模型判断（说法可能和当初记的不一样），代码不按内容打分；删除只收编号。
// listLimit 是一次最多交给模型的条数，超过时结果里写明一共几条、只给了几条。
func NoteTools(store notes.Repository, listLimit int, logger *slog.Logger) Builtin {
	logger = logger.With("tool", "notes")
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: "save_note", Annotations: Changes,
			Description: "把主人要求记下的事情保存到记事本。记下来不等于会提醒：要到点说出来的事用定提醒的工具，不是记事；" +
				"记完只说「记下了」，不要说「到时候提醒你」。"},
			func(ctx context.Context, in saveNoteInput) (noteOutput, error) {
				note, err := store.Save(ctx, in.Content)
				logger.Info("记一条", "id", note.ID, "err", err)
				return noteOutput{Note: noteView(note)}, err
			})
		AddTool(server, &mcp.Tool{Name: "search_notes", Annotations: ReadOnly,
			Description: "列出记事本里的记录（带编号，新的在前）。主人问以前让你记过的事、要找或要删某一条时用：" +
				"你自己从里面找出主人说的那条，说法可能和当初记的不一样，按意思找。主人问以前让你记过的事（东西放在哪、车停在哪、约了什么）时先查这里，不要凭画面或印象猜。"},
			func(ctx context.Context, _ struct{}) (noteListOutput, error) {
				list, total, err := store.List(ctx, listLimit)
				if err != nil {
					return noteListOutput{}, err
				}
				out := noteListOutput{Notes: noteViews(list), Total: total}
				if total > len(list) {
					out.Note = "记事本里一共有这么多条，只给了最近的这些；更早的看不到。"
					logger.Warn("记事超过一次能给的条数：只给了最近的", "total", total, "shown", len(list))
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: "delete_note", Annotations: Destructive,
			Description: "按编号删除记事本里的记录（可以恢复），一次可以删多条。先用 search_notes 找到要删的那几条的编号。" +
				"结果里 not_found 是没有的编号，要如实告诉主人。"},
			func(ctx context.Context, in deleteNotesInput) (deleteNotesOutput, error) {
				if len(in.IDs) == 0 {
					return deleteNotesOutput{}, errors.New("ids 不能为空：要说清楚删哪几条")
				}
				out := deleteNotesOutput{Deleted: []noteJSON{}, NotFound: []int64{}}
				for _, id := range in.IDs {
					note, err := store.Delete(ctx, id)
					logger.Info("删除", "id", id, "err", err)
					switch {
					case errors.Is(err, notes.ErrNotFound):
						out.NotFound = append(out.NotFound, id)
					case err != nil:
						return out, err
					default:
						out.Deleted = append(out.Deleted, noteView(note))
					}
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: "list_deleted_notes", Annotations: ReadOnly,
			Description: "列出最近删掉的记录（带编号，最近删的在前）。主人想找回删掉的东西时用。"},
			func(ctx context.Context, _ struct{}) (noteListOutput, error) {
				list, err := store.Deleted(ctx, deletedListLimit)
				return noteListOutput{Notes: noteViews(list), Total: len(list)}, err
			})
		AddTool(server, &mcp.Tool{Name: "restore_note", Annotations: Changes,
			Description: "按编号恢复一条删掉的记录。先用 list_deleted_notes 找到它的编号。"},
			func(ctx context.Context, in restoreNoteInput) (noteOutput, error) {
				note, err := store.Restore(ctx, in.ID)
				logger.Info("恢复", "id", in.ID, "err", err)
				if errors.Is(err, notes.ErrNotFound) {
					return noteOutput{}, errors.New("没有这个编号的已删除记录")
				}
				return noteOutput{Note: noteView(note)}, err
			})
	}
}

type saveNoteInput struct {
	Content string `json:"content" jsonschema:"要记的内容（主人的原话）"`
}

type deleteNotesInput struct {
	IDs []int64 `json:"ids" jsonschema:"要删的记录的编号（search_notes 结果里的 id），一次可以多个"`
}

type restoreNoteInput struct {
	ID int64 `json:"id" jsonschema:"要恢复的记录的编号（list_deleted_notes 结果里的 id）"`
}

// noteJSON 是交给模型的一条记录。
type noteJSON struct {
	ID        int64  `json:"id"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
	DeletedAt string `json:"deleted_at,omitempty"`
}

type noteOutput struct {
	Note noteJSON `json:"note"`
}

type noteListOutput struct {
	Notes []noteJSON `json:"notes"`
	Total int        `json:"total"`          // 一共有几条
	Note  string     `json:"note,omitempty"` // 没全给时的说明
}

type deleteNotesOutput struct {
	Deleted  []noteJSON `json:"deleted"`
	NotFound []int64    `json:"not_found"`
}

func noteView(note notes.Note) noteJSON {
	view := noteJSON{ID: note.ID, Content: note.Content, CreatedAt: localTime(note.CreatedAt)}
	if !note.DeletedAt.IsZero() {
		view.DeletedAt = localTime(note.DeletedAt)
	}
	return view
}

func noteViews(list []notes.Note) []noteJSON {
	views := make([]noteJSON, 0, len(list))
	for _, note := range list {
		views = append(views, noteView(note))
	}
	return views
}

// localTime 是给模型看的时间（本地时区，到分钟）。
func localTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}
