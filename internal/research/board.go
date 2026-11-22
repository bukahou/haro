package research

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bukahou/haro/internal/brain"
)

// 任务板与状态：每轮告诉主对话的模型调查的情况（Brief），以及主对话的 research_status 查到的内容（Status）。

// 主对话里三个调查工具的名字（工具注册在 internal/tools；任务板、说明里提到它们时引用这里）。
const (
	StartTool  = "start_research"
	StatusTool = "research_status"
	CancelTool = "cancel_research"
)

// statusRecent 是不给编号时列出最近几个调查。
const statusRecent = 5

// StatusReport 是 research_status 交给模型的结果：调查（新的在前）和附注。
type StatusReport struct {
	Tasks []Task `json:"tasks"`
	Note  string `json:"note,omitempty"`
}

// Status 是某人的调查的进度和结论：给编号就是那一个，不给就是最近几个。列出了还没告诉过的结论（或失败）：
// 这一轮主人听完才标成已告诉（brain.OnDelivered；被打断就之后再附在任务板上）。
func (r *Runner) Status(ctx context.Context, person string, id *int64) (StatusReport, error) {
	var tasks []Task
	if id != nil {
		task, err := r.store.Get(ctx, *id)
		if errors.Is(err, ErrTaskNotFound) || err == nil && task.Person != person {
			return StatusReport{Tasks: []Task{}, Note: "没有这个编号的调查"}, nil
		}
		if err != nil {
			return StatusReport{}, err
		}
		tasks = []Task{task}
	} else {
		recent, err := r.store.Recent(ctx, person, statusRecent)
		if err != nil {
			return StatusReport{}, err
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
		return StatusReport{Tasks: []Task{}, Note: "没有调查记录"}, nil
	}
	return StatusReport{Tasks: tasks}, nil
}

// markReported 把这些调查标成已告诉主人（之后不再附在任务板上）。在那一轮结束之后调用，用自己的期限。
func (r *Runner) markReported(ids []int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.store.MarkReported(ctx, ids...); err != nil {
		r.logger.Warn("标记调查已汇报失败：之后会再说一次", "tasks", ids, "err", err)
	}
}

// boardUnreadableNote 是任务板这一轮没读出来时给模型的附注（不知道有没有查完的调查，不能当成没有）。
const boardUnreadableNote = "后台调查的任务板这一轮没读出来（存储出错）：不知道有没有查完的调查，不用主动提；" +
	"主人问起时照实说，可以用 " + StatusTool + " 再查一次。"

// Brief 实现 brain.Briefer：任务板。在查的列出来；查完还没告诉的，让模型这一轮先用 research_status 取结论讲给主人
// （结论不放在这里：取了、主人也听完了才标已汇报，见 Status）；没完成的，原因短，直接附上，主人听完这一轮就算告诉了。
// 读不出来时记警告，告诉模型这一轮不知道调查的情况。
func (r *Runner) Brief(person string) brain.Briefing {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	board, err := r.store.Board(ctx, person)
	if err != nil {
		r.logger.Warn("任务板没读出来：这一轮告诉模型不知道调查的情况", "person", person, "err", err)
		return brain.Briefing{Lines: []string{boardUnreadableNote}}
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
				"（按需要的长短说，讲清楚为止），再回答他这次说的话。", task.ID, task.Question, note, StatusTool, task.ID))
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
