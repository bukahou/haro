package research

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
				// 告诉模型怎么跟主人说：它不能主动开口，查好了终端只会响一声（2026-11-17：模型说「我盯紧了给你捡漏」）
				const howToTell = "照实告诉主人：查好了终端会「叮」一声，到时叫醒你问就行（你不会自己说结论）；不要说会盯着、守着。"
				if ahead > 0 {
					return fmt.Sprintf("已派出调查 #%d，前面还有 %d 个在查或排队，要多等一会儿。%s", task.ID, ahead, howToTell), nil
				}
				return fmt.Sprintf("已派出调查 #%d，一般一两分钟。%s", task.ID, howToTell), nil
			}},
		{Spec: brain.ToolSpec{Name: "research_status",
			Description: "看后台调查的进度和结论。主人问「查得怎么样了」「上次让你查的 X 结果是什么」时用；query 填关键词可以找以前的调查。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"可选：找以前某个调查的关键词"}}}`)},
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

// status 列出某人最近的调查（或按关键词找），并把列出了结论的标成已告诉。
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
	var reported []int64
	for _, task := range tasks {
		if task.Status.Finished() {
			reported = append(reported, task.ID)
		}
	}
	_ = r.store.MarkReported(ctx, reported...)
	data, err := json.Marshal(map[string]any{"tasks": tasks})
	return string(data), err
}

// briefingConclusionRunes 是任务板里每条结论最多带多少字（太长的让模型调 research_status 取全文）。
const briefingConclusionRunes = 400

// Brief 实现 brain.Briefer：任务板（R3 a / R4）。在查的列出来；查完还没告诉的，把结论放进这一轮，让模型先说。
// 放进去就算告诉过了（每条只出现一次，以后要看用 research_status）。
func (r *Runner) Brief(person string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	board, err := r.store.Board(ctx, person)
	if err != nil {
		return nil
	}
	var lines []string
	var told []int64
	for _, task := range board {
		if !task.Status.Finished() {
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」%s（派出 %s 了），不用主动提。",
				task.ID, task.Question, task.Status.Label(), since(task.CreatedAt)))
			continue
		}
		told = append(told, task.ID)
		switch task.Status {
		case StatusDone, StatusPartial:
			note := ""
			if task.Status == StatusPartial {
				note = "（到上限收工，可能不全：" + task.StopReason + "）"
			}
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」查完了%s。这一轮先把结论告诉主人（按需要的长短说，讲清楚为止），再回答他这次说的话。结论：%s",
				task.ID, task.Question, note, clip(task.Conclusion, briefingConclusionRunes)))
		default:
			lines = append(lines, fmt.Sprintf("后台调查 #%d「%s」没完成（%s：%s）。这一轮顺便告诉主人一声。",
				task.ID, task.Question, task.Status.Label(), task.StopReason))
		}
	}
	_ = r.store.MarkReported(ctx, told...)
	return lines
}

func since(t time.Time) string {
	elapsed := time.Since(t).Round(time.Second)
	if elapsed < time.Minute {
		return fmt.Sprintf("%d 秒", int(elapsed.Seconds()))
	}
	return fmt.Sprintf("%d 分钟", int(elapsed.Minutes()))
}

func clip(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…（全文用 research_status 看）"
}

func stringArgument(arguments map[string]any, key string) string {
	value, _ := arguments[key].(string)
	return value
}
