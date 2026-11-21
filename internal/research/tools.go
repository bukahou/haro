package research

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/brain"
	"github.com/bukahou/haro/internal/tools"
)

// Tools 返回主对话里的三个调查工具（R1：复杂问题派后台调查；简单的用 web_search 当场答）。
func (r *Runner) Tools() []tools.Builtin {
	return []tools.Builtin{
		{Spec: brain.ToolSpec{Name: "start_research",
			Description: "派一个后台调查：需要多搜几次、读几篇资料、比较或汇总的问题用它（例如「帮我调查一下 A 和 B 哪个好」「查查 X 最近的情况」）。" +
				"调用后马上告诉主人已经开始查、查好了再说，然后可以继续聊别的。一次就能搜到答案的问题用 web_search，不用它。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","description":"要调查的问题，写完整、具体（包含主人关心的点）"}},"required":["question"]}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				question, _ := arguments["question"].(string)
				task, ahead, err := r.Start(ctx, brain.SpeakerFrom(ctx), question)
				if err != nil {
					return "", err
				}
				// 告诉模型怎么跟主人说：它不能主动开口，查好了终端只会响一声（不说明的话，模型会答应盯着、一有消息就说）
				const howToTell = "照实告诉主人：查好了终端会「叮」一声，到时叫醒你问就行（你不会自己说结论）；不要说会盯着、守着。"
				if ahead > 0 {
					return fmt.Sprintf("已派出调查 #%d，前面还有 %d 个在查或排队，要多等一会儿。%s", task.ID, ahead, howToTell), nil
				}
				return fmt.Sprintf("已派出调查 #%d，一般一两分钟。%s", task.ID, howToTell), nil
			}},
		{Spec: brain.ToolSpec{Name: "research_status",
			Description: "看后台调查的进度和结论。主人问「查得怎么样了」「上次让你查的 X 结果是什么」时用；" +
				"情况说明里说有调查查完了、还没告诉主人时，也先用它取结论再讲。query 填关键词可以找以前的调查。",
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"可选：找以前某个调查的关键词"}}}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				return r.status(ctx, brain.SpeakerFrom(ctx), stringArgument(arguments, "query"))
			}},
		{Spec: brain.ToolSpec{Name: "cancel_research",
			Description: "取消后台调查（主人说「别查了」「不用查了」时）。不给 id 就取消最近一个还没查完的。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer","description":"可选：调查编号"}}}`)},
			Run: func(ctx context.Context, arguments map[string]any) (string, error) {
				id, _ := arguments["id"].(float64)
				task, err := r.Cancel(ctx, brain.SpeakerFrom(ctx), int64(id))
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("调查 #%d「%s」已取消。", task.ID, task.Question), nil
			}},
	}
}

// status 列出某人最近的调查（或按关键词找）。列出了还没告诉过的结论（或失败）：这一轮主人听完才标成已告诉
// （brain.OnDelivered；被打断就之后再附在任务板上）。
func (r *Runner) status(ctx context.Context, person, query string) (string, error) {
	var tasks []Task
	var err error
	if strings.TrimSpace(query) != "" {
		tasks, err = r.store.Search(ctx, person, query, 3)
	} else {
		tasks, err = r.store.Recent(ctx, person, 5)
	}
	if err != nil {
		return "", err
	}
	if len(tasks) == 0 {
		return `{"tasks":[],"note":"没有找到调查记录"}`, nil
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
	data, err := json.Marshal(map[string]any{"tasks": tasks})
	return string(data), err
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
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」查完了%s，还没告诉主人。这一轮先调用 research_status 取结论讲给主人"+
				"（按需要的长短说，讲清楚为止），再回答他这次说的话。", task.ID, task.Question, note))
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

func stringArgument(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return value
}
