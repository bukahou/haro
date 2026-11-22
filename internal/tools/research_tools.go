package tools

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/research"
)

// ResearchDesk 是主对话的调查工具要用的（research.Runner 实现）。
type ResearchDesk interface {
	Start(ctx context.Context, person, question string) (research.Task, int, error)
	Status(ctx context.Context, person string, id *int64) (research.StatusReport, error)
	Cancel(ctx context.Context, person string, id int64) (research.Task, error)
	LongestWait() time.Duration
}

// ResearchTools 是主对话里的三个调查工具：派调查、看进度和结论、取消（复杂问题派后台调查；简单的用 web_search 当场答）。
// 看哪一个、取消哪一个按编号：编号在任务板（research.Runner.Brief）和 research_status 的结果里。
func ResearchTools(desk ResearchDesk) Builtin {
	return func(server *mcp.Server) {
		AddTool(server, &mcp.Tool{Name: research.StartTool, Annotations: Changes,
			Description: "派一个后台调查：需要多搜几次、读几篇资料、比较或汇总的问题用它（例如「帮我调查一下 A 和 B 哪个好」「查查 X 最近的情况」）。" +
				"调用后马上告诉主人已经开始查、查好了再说，然后可以继续聊别的。一次就能搜到答案的问题用 " + WebSearchTool + "，不用它。"},
			func(ctx context.Context, in startResearchInput) (startResearchOutput, error) {
				task, ahead, err := desk.Start(ctx, brain.SpeakerFrom(ctx), in.Question)
				if err != nil {
					return startResearchOutput{}, err
				}
				// 告诉模型怎么跟主人说：它不能主动开口，查好了终端只会响一声（不说明的话，模型会答应盯着、一有消息就说）
				out := startResearchOutput{ID: task.ID, Ahead: ahead,
					HowToTell: "照实告诉主人：查好了终端会「叮」一声，到时叫醒你问就行（你不会自己说结论）；不要说会盯着、守着。"}
				if ahead > 0 {
					out.Note = "前面还有别的调查在查或排队，要多等一会儿。"
				} else {
					out.Note = longestWaitNote(desk.LongestWait())
				}
				return out, nil
			})
		AddTool(server, &mcp.Tool{Name: research.StatusTool, Annotations: ReadOnly,
			Description: "看后台调查的进度和结论：给 id 看那一个；不给就列出最近的几个（带编号）。" +
				"主人问「查得怎么样了」「上次让你查的那个结果是什么」时用；情况说明里说有调查查完了、还没告诉主人时，也先用它取结论再讲。"},
			func(ctx context.Context, in researchStatusInput) (research.StatusReport, error) {
				return desk.Status(ctx, brain.SpeakerFrom(ctx), in.ID)
			})
		AddTool(server, &mcp.Tool{Name: research.CancelTool, Annotations: Changes,
			Description: "取消后台调查（主人说「别查了」「不用查了」时）。不给 id 就取消最近一个还没查完的。"},
			func(ctx context.Context, in cancelResearchInput) (cancelResearchOutput, error) {
				var id int64
				if in.ID != nil {
					id = *in.ID
				}
				task, err := desk.Cancel(ctx, brain.SpeakerFrom(ctx), id)
				if err != nil {
					return cancelResearchOutput{}, err
				}
				return cancelResearchOutput{ID: task.ID, Question: task.Question, Status: "已取消"}, nil
			})
	}
}

// longestWaitNote 是派出后告诉主人最长要等多久（分钟，往上取整；到时限就用查到的资料收尾）。
func longestWaitNote(longest time.Duration) string {
	return fmt.Sprintf("最长约 %d 分钟（到时限就用已经查到的资料收尾）。", int(math.Ceil(longest.Minutes())))
}

type startResearchInput struct {
	Question string `json:"question" jsonschema:"要调查的问题，用主人说话的语言写完整、具体（包含主人关心的点）"`
}

type startResearchOutput struct {
	ID        int64  `json:"id"`
	Ahead     int    `json:"ahead"` // 前面还有几个在查或排队
	Note      string `json:"note"`
	HowToTell string `json:"how_to_tell"`
}

type researchStatusInput struct {
	ID *int64 `json:"id,omitempty" jsonschema:"可选：调查的编号；不给就列出最近的几个"`
}

type cancelResearchInput struct {
	ID *int64 `json:"id,omitempty" jsonschema:"可选：调查的编号；不给就取消最近一个还没查完的"`
}

type cancelResearchOutput struct {
	ID       int64  `json:"id"`
	Question string `json:"question"`
	Status   string `json:"status"`
}
