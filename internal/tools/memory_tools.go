package tools

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/conversationstore"
	"github.com/bukahou/haro/internal/memory"
	"github.com/bukahou/haro/internal/notes"
)

// MemoryDesk 是长期记忆的工具要用的（memory.Keeper 实现）。
type MemoryDesk interface {
	Remember(ctx context.Context, person, content string, source notes.Source, resident bool) (notes.Note, error)
	Recall(ctx context.Context, person, query string) (memory.Recollection, error)
	List(ctx context.Context, person string, withForgotten bool) (memory.Listing, error)
	Forget(ctx context.Context, person string, id int64) (notes.Note, error)
	Restore(ctx context.Context, person string, id int64) (notes.Note, error)
}

// 长期记忆的工具名（只在这里定义一处）。
const (
	RememberTool      = "remember"
	RecallTool        = "recall"
	ListMemoriesTool  = "list_memories"
	ForgetTool        = "forget"
	RestoreMemoryTool = "restore_memory"
)

// errMemoryNotVerified：这句话的声纹没认出是登记过的人（v10 D6：只有本人的声音能记、恢复；忘掉走确认闸，闸自己会查）。
var errMemoryNotVerified = errors.New("这句话没认出是登记过的人说的：只有本人的声音能让你记住、恢复。照实告诉主人，请他再说一次")

// MemoryTools 是长期记忆的五个工具（v10，记事已并进来）：记住、按意思找、列出、忘掉（会破坏，过确认闸）、恢复。
// 来源（主人让记的 / 你觉得该记的）和要不要常驻由模型判断，代码不看内容；找哪一条、相关不相关也由模型判断。
// 按人：只看得到、只改得了自己的和全家共用的。时间按 location 写给模型看。
func MemoryTools(desk MemoryDesk, location *time.Location, logger *slog.Logger) Builtin {
	logger = logger.With("tool", "memory")
	view := func(note notes.Note) memoryJSON { return memoryView(note, location) }
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: RememberTool, Annotations: Changes,
			Description: "记住一件以后用得上的事。主人说「记一下」「你记住」时 source 填 owner；主人顺口说出的、以后用得上的偏好或事实" +
				"（例如「我不吃香菜」「我周三固定加班」），你觉得该记的，source 填 model（不用先问）。称呼、忌口、作息这类每次说话都该记着的，" +
				"resident 填 true（会每轮附在情况说明里）。不记住址、健康状况、证件号、密码、银行卡这类敏感信息，也不记别人的隐私。" +
				"记下来不等于会提醒：要到点说出来的事用定提醒的工具。记完简短说一句「记住了」。"},
			func(ctx context.Context, in rememberInput) (memoryOutput, error) {
				if !brain.VoiceVerifiedFrom(ctx) {
					return memoryOutput{}, errMemoryNotVerified
				}
				source := notes.Source(in.Source)
				if !source.Valid() {
					return memoryOutput{}, brain.InvalidArguments(errors.New("source 只能是 owner（主人让记的）或 model（你觉得该记的）"))
				}
				note, err := desk.Remember(ctx, brain.SpeakerFrom(ctx), in.Content, source, in.Resident)
				if errors.Is(err, memory.ErrTooLong) { // 压短了再记就行
					return memoryOutput{}, brain.InvalidArguments(err)
				}
				if err != nil {
					return memoryOutput{}, err
				}
				return memoryOutput{Memory: view(note)}, nil
			})
		AddTool(server, &mcp.Tool{Name: RecallTool, Annotations: ReadOnly,
			Description: "按意思找以前记住的事和以前的对话（几天、几周前聊过的也找得到）。主人问以前的事（「我车停哪了」「我上次说的那家店」" +
				"「我们之前聊过 X 吗」），或者要回答的问题可能和以前记的有关时用，不要凭印象猜。结果里 memories 是记住的事，" +
				"conversations 是以前的对话（时间、主人说的、你答的）：你自己判断哪些相关，不相关的不要提。"},
			func(ctx context.Context, in recallInput) (recallOutput, error) {
				if in.Query == "" {
					return recallOutput{}, brain.InvalidArguments(errors.New("query 不能为空：写要找什么"))
				}
				recollection, err := desk.Recall(ctx, brain.SpeakerFrom(ctx), in.Query)
				if err != nil {
					return recallOutput{}, err
				}
				out := recallOutput{Memories: []memoryJSON{}, Conversations: []pastTurnJSON{}, Note: recollection.Note}
				for _, note := range recollection.Notes {
					out.Memories = append(out.Memories, view(note))
				}
				for _, turn := range recollection.Turns {
					out.Conversations = append(out.Conversations, pastTurnView(turn, location))
				}
				logger.Info("按意思找", "memories", len(out.Memories), "conversations", len(out.Conversations), "by_meaning", recollection.ByMeaning)
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: ListMemoriesTool, Annotations: ReadOnly,
			Description: "列出记住的事（带编号，新的在前）。主人问「你记得我什么」、要忘掉或找回某一条时用；" +
				"要找回忘掉的，include_forgotten 填 true。"},
			func(ctx context.Context, in listMemoriesInput) (memoryListOutput, error) {
				listing, err := desk.List(ctx, brain.SpeakerFrom(ctx), in.IncludeForgotten)
				if err != nil {
					return memoryListOutput{}, err
				}
				out := memoryListOutput{Memories: []memoryJSON{}, Total: listing.Total}
				for _, note := range listing.Notes {
					out.Memories = append(out.Memories, view(note))
				}
				if listing.Total > len(listing.Notes) {
					out.Note = "一共有这么多条，只给了最近的这些；更早的用 " + RecallTool + " 按意思找。"
				}
				if in.IncludeForgotten {
					out.Forgotten = []memoryJSON{}
					for _, note := range listing.Forgotten {
						out.Forgotten = append(out.Forgotten, view(note))
					}
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: ForgetTool, Annotations: Destructive,
			Description: "按编号忘掉记住的事（可以恢复），一次可以多条。先用 " + RecallTool + " 或 " + ListMemoriesTool + " 找到编号。" +
				"结果里 not_found 是没有的编号，要如实告诉主人。"},
			func(ctx context.Context, in forgetInput) (forgetOutput, error) {
				if len(in.IDs) == 0 {
					return forgetOutput{}, brain.InvalidArguments(errors.New("ids 不能为空：要说清楚忘掉哪几条"))
				}
				out := forgetOutput{Forgotten: []memoryJSON{}, NotFound: []int64{}}
				for _, id := range in.IDs {
					note, err := desk.Forget(ctx, brain.SpeakerFrom(ctx), id)
					switch {
					case errors.Is(err, notes.ErrNotFound):
						out.NotFound = append(out.NotFound, id)
					case err != nil:
						return out, err
					default:
						out.Forgotten = append(out.Forgotten, view(note))
					}
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: RestoreMemoryTool, Annotations: Changes,
			Description: "按编号恢复忘掉的事。先用 " + ListMemoriesTool + "（include_forgotten 填 true）找到编号。"},
			func(ctx context.Context, in restoreMemoryInput) (memoryOutput, error) {
				if !brain.VoiceVerifiedFrom(ctx) {
					return memoryOutput{}, errMemoryNotVerified
				}
				note, err := desk.Restore(ctx, brain.SpeakerFrom(ctx), in.ID)
				if errors.Is(err, notes.ErrNotFound) {
					return memoryOutput{}, brain.InvalidArguments(errors.New("没有这个编号的、忘掉了的记忆"))
				}
				if err != nil {
					return memoryOutput{}, err
				}
				return memoryOutput{Memory: view(note)}, nil
			})
	}
}

type rememberInput struct {
	Content  string `json:"content" jsonschema:"要记住的事（一两句要点）"`
	Source   string `json:"source" jsonschema:"owner = 主人让记的；model = 你觉得该记的"`
	Resident bool   `json:"resident,omitempty" jsonschema:"每次说话都该记着的（称呼、忌口、作息这类）：每轮附在情况说明里"`
}

type recallInput struct {
	Query string `json:"query" jsonschema:"要找什么（用一句话说清楚，例如「主人的车停在哪」）"`
}

type listMemoriesInput struct {
	IncludeForgotten bool `json:"include_forgotten,omitempty" jsonschema:"同时列出最近忘掉的（找回用）"`
}

type forgetInput struct {
	IDs []int64 `json:"ids" jsonschema:"要忘掉的编号（recall 或 list_memories 结果里的 id），一次可以多个"`
}

type restoreMemoryInput struct {
	ID int64 `json:"id" jsonschema:"要恢复的编号（list_memories 带上 include_forgotten 时结果里的 id）"`
}

// memoryJSON 是交给模型的一条记忆（时间按服务端的时区写）。
type memoryJSON struct {
	ID          int64  `json:"id"`
	Content     string `json:"content"`
	Source      string `json:"source"`             // owner / model
	Resident    bool   `json:"resident,omitempty"` // 常驻
	Shared      bool   `json:"shared,omitempty"`   // 全家共用（以前的记事）
	CreatedAt   string `json:"created_at"`
	ForgottenAt string `json:"forgotten_at,omitempty"`
}

// pastTurnJSON 是交给模型的以前的一轮对话。
type pastTurnJSON struct {
	At       string `json:"at,omitempty"` // 不知道时不写
	Said     string `json:"said,omitempty"`
	Answered string `json:"answered,omitempty"`
}

type memoryOutput struct {
	Memory memoryJSON `json:"memory"`
}

type recallOutput struct {
	Memories      []memoryJSON   `json:"memories"`
	Conversations []pastTurnJSON `json:"conversations"`
	Note          string         `json:"note,omitempty"`
}

type memoryListOutput struct {
	Memories  []memoryJSON `json:"memories"`
	Total     int          `json:"total"`
	Note      string       `json:"note,omitempty"`
	Forgotten []memoryJSON `json:"forgotten,omitempty"`
}

type forgetOutput struct {
	Forgotten []memoryJSON `json:"forgotten"`
	NotFound  []int64      `json:"not_found"`
}

func memoryView(note notes.Note, location *time.Location) memoryJSON {
	view := memoryJSON{ID: note.ID, Content: note.Content, Source: string(note.Source), Resident: note.Resident,
		Shared: note.Person == "", CreatedAt: note.CreatedAt.In(location).Format(time.RFC3339)}
	if !note.DeletedAt.IsZero() {
		view.ForgottenAt = note.DeletedAt.In(location).Format(time.RFC3339)
	}
	return view
}

func pastTurnView(turn conversationstore.TranscriptTurn, location *time.Location) pastTurnJSON {
	view := pastTurnJSON{Said: turn.Said, Answered: turn.Answered}
	if !turn.At.IsZero() {
		view.At = turn.At.In(location).Format(time.RFC3339)
	}
	return view
}
