package research

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/tools"
)

// 主对话里三个调查工具的名字（任务板、说明里提到它们时引用这里）。
const (
	startResearchTool  = "start_research"
	researchStatusTool = "research_status"
	cancelResearchTool = "cancel_research"
)

// Tools 是主对话里的三个调查工具：派调查、看进度和结论、取消（复杂问题派后台调查；简单的用 web_search 当场答）。
// 看哪一个、取消哪一个按编号：编号在任务板（Brief）和 research_status 的结果里。
func (r *Runner) Tools() tools.Builtin {
	return func(server *mcp.Server) {
		tools.AddTool(server, &mcp.Tool{Name: startResearchTool, Annotations: tools.Changes,
			Description: "派一个后台调查：需要多搜几次、读几篇资料、比较或汇总的问题用它（例如「帮我调查一下 A 和 B 哪个好」「查查 X 最近的情况」）。" +
				"调用后马上告诉主人已经开始查、查好了再说，然后可以继续聊别的。一次就能搜到答案的问题用 " + tools.WebSearchTool + "，不用它。"},
			func(ctx context.Context, in startInput) (startOutput, error) {
				task, ahead, err := r.Start(ctx, brain.SpeakerFrom(ctx), in.Question)
				if err != nil {
					return startOutput{}, err
				}
				// 告诉模型怎么跟主人说：它不能主动开口，查好了终端只会响一声（不说明的话，模型会答应盯着、一有消息就说）
				out := startOutput{ID: task.ID, Ahead: ahead,
					HowToTell: "照实告诉主人：查好了终端会「叮」一声，到时叫醒你问就行（你不会自己说结论）；不要说会盯着、守着。"}
				if ahead > 0 {
					out.Note = "前面还有别的调查在查或排队，要多等一会儿。"
				} else {
					out.Note = r.longestNote()
				}
				return out, nil
			})
		tools.AddTool(server, &mcp.Tool{Name: researchStatusTool, Annotations: tools.ReadOnly,
			Description: "看后台调查的进度和结论：给 id 看那一个；不给就列出最近的几个（带编号）。" +
				"主人问「查得怎么样了」「上次让你查的那个结果是什么」时用；情况说明里说有调查查完了、还没告诉主人时，也先用它取结论再讲。"},
			func(ctx context.Context, in statusInput) (statusOutput, error) {
				return r.lookUpStatus(ctx, brain.SpeakerFrom(ctx), in.ID)
			})
		tools.AddTool(server, &mcp.Tool{Name: cancelResearchTool, Annotations: tools.Changes,
			Description: "取消后台调查（主人说「别查了」「不用查了」时）。不给 id 就取消最近一个还没查完的。"},
			func(ctx context.Context, in cancelInput) (cancelOutput, error) {
				var id int64
				if in.ID != nil {
					id = *in.ID
				}
				task, err := r.Cancel(ctx, brain.SpeakerFrom(ctx), id)
				if err != nil {
					return cancelOutput{}, err
				}
				return cancelOutput{ID: task.ID, Question: task.Question, Status: "已取消"}, nil
			})
	}
}

// longestNote 是派出后告诉主人最长要等多久：搜索阶段的时限加上写结论的时间（到时限就用查到的资料收尾）。
func (r *Runner) longestNote() string {
	longest := r.limits.Timeout + writeUpTimeout
	return fmt.Sprintf("最长约 %d 分钟（到时限就用已经查到的资料收尾）。", int(math.Ceil(longest.Minutes())))
}

type startInput struct {
	Question string `json:"question" jsonschema:"要调查的问题，用主人说话的语言写完整、具体（包含主人关心的点）"`
}

type startOutput struct {
	ID        int64  `json:"id"`
	Ahead     int    `json:"ahead"` // 前面还有几个在查或排队
	Note      string `json:"note"`
	HowToTell string `json:"how_to_tell"`
}

type statusInput struct {
	ID *int64 `json:"id,omitempty" jsonschema:"可选：调查的编号；不给就列出最近的几个"`
}

type statusOutput struct {
	Tasks []Task `json:"tasks"`
	Note  string `json:"note,omitempty"`
}

type cancelInput struct {
	ID *int64 `json:"id,omitempty" jsonschema:"可选：调查的编号；不给就取消最近一个还没查完的"`
}

type cancelOutput struct {
	ID       int64  `json:"id"`
	Question string `json:"question"`
	Status   string `json:"status"`
}

// statusRecent 是不给编号时列出最近几个调查。
const statusRecent = 5

// lookUpStatus 给编号就是那一个，不给就是最近几个。列出了还没告诉过的结论（或失败）：这一轮主人听完才标成已告诉
// （brain.OnDelivered；被打断就之后再附在任务板上）。
func (r *Runner) lookUpStatus(ctx context.Context, person string, id *int64) (statusOutput, error) {
	var tasks []Task
	if id != nil {
		task, err := r.store.Get(ctx, *id)
		if errors.Is(err, ErrTaskNotFound) || err == nil && task.Person != person {
			return statusOutput{Tasks: []Task{}, Note: "没有这个编号的调查"}, nil
		}
		if err != nil {
			return statusOutput{}, err
		}
		tasks = []Task{task}
	} else {
		recent, err := r.store.Recent(ctx, person, statusRecent)
		if err != nil {
			return statusOutput{}, err
		}
		tasks = recent
	}
	var told []int64
	for _, task := range tasks {
		if task.Status.Finished() && !task.Reported {
			told = append(told, task.ID)
		}
	}
	if len(told) > 0 {
		brain.OnDelivered(ctx, func() { r.markReported(told) })
	}
	if tasks == nil {
		return statusOutput{Tasks: []Task{}, Note: "没有调查记录"}, nil
	}
	return statusOutput{Tasks: tasks}, nil
}

// markReported 把这些调查标成已告诉主人（之后不再附在任务板上）。在那一轮结束之后调用，用自己的期限。
func (r *Runner) markReported(ids []int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.store.MarkReported(ctx, ids...); err != nil {
		r.logger.Warn("标记调查已汇报失败：之后会再说一次", "tasks", ids, "err", err)
	}
}

// Brief 实现 brain.Briefer：任务板。在查的列出来；查完还没告诉的，让模型这一轮先用 research_status 取结论讲给主人
// （结论不放在这里：取了、主人也听完了才标已汇报，见 status）；没完成的，原因短，直接附上，主人听完这一轮就算告诉了。
func (r *Runner) Brief(person string) brain.Briefing {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	board, err := r.store.Board(ctx, person)
	if err != nil {
		return brain.Briefing{}
	}
	var lines []string
	var told []int64
	for _, task := range board {
		if !task.Status.Finished() {
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」%s（派出 %s 了），不用主动提。",
				task.ID, task.Question, task.Status.Label(), since(task.CreatedAt)))
			continue
		}
		switch task.Status {
		case StatusDone, StatusPartial:
			note := ""
			if task.Status == StatusPartial {
				note = "（到上限收工，可能不全：" + task.StopReason + "）"
			}
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」查完了%s，还没告诉主人。这一轮先调用 %s（id 填 %d）取结论讲给主人"+
				"（按需要的长短说，讲清楚为止），再回答他这次说的话。", task.ID, task.Question, note, researchStatusTool, task.ID))
		default:
			told = append(told, task.ID)
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」没完成（%s：%s）。这一轮顺便告诉主人一声。",
				task.ID, task.Question, task.Status.Label(), task.StopReason))
		}
	}
	briefing := brain.Briefing{Lines: lines}
	if len(told) > 0 {
		briefing.Delivered = func() { r.markReported(told) }
	}
	return briefing
}

func since(t time.Time) string {
	elapsed := time.Since(t).Round(time.Second)
	if elapsed < time.Minute {
		return fmt.Sprintf("%d 秒", int(elapsed.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(elapsed.Minutes()))
}
