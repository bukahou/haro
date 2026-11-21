// Package research 是后台调查（v5 第 3 节）：主对话派任务、立刻回话；调查在服务端进程的后台跑，
// 受程序强制的上限约束（时间 / 轮数 / 搜索次数 / token，R2），结论写进任务账本；
// 每轮对话的系统提示词里放「任务板」（R3 a / R4），服务端不会忘了自己派过什么、查到了什么。
//
// 对外：OpenTaskStore（Facade）+ TaskStore（接口）；Runner（排队与执行）；Tools（主对话的工具）；Runner 兼任 brain.Briefer。
package research

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bukahou/haro/internal/sqliteschema"
)

// Status 是任务状态。
type Status string

const (
	StatusQueued      Status = "queued"      // 排队中
	StatusRunning     Status = "running"     // 在查
	StatusDone        Status = "done"        // 查完了
	StatusPartial     Status = "partial"     // 到上限收工，结论可能不全
	StatusFailed      Status = "failed"      // 出错（模型 / 搜索服务不可用）
	StatusCancelled   Status = "cancelled"   // 主人说不查了
	StatusInterrupted Status = "interrupted" // 服务端重启，没查完
)

// Label 是给模型看的中文状态。
func (s Status) Label() string {
	switch s {
	case StatusQueued:
		return "排队中"
	case StatusRunning:
		return "正在查"
	case StatusDone:
		return "查完了"
	case StatusPartial:
		return "查了一部分（到上限收工）"
	case StatusFailed:
		return "失败了"
	case StatusCancelled:
		return "取消了"
	case StatusInterrupted:
		return "被中断了（服务端重启）"
	default:
		return string(s)
	}
}

// Finished 判断任务是不是已经结束。
func (s Status) Finished() bool { return s != StatusQueued && s != StatusRunning }

// Task 是一个调查任务。
type Task struct {
	ID         int64     `json:"id"`
	Person     string    `json:"-"`
	Question   string    `json:"question"`
	Status     Status    `json:"-"`
	StatusText string    `json:"status"`
	Conclusion string    `json:"conclusion,omitempty"`
	Sources    []string  `json:"sources,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"` // 为什么没查完 / 为什么失败
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"-"`
	Reported   bool      `json:"-"` // 结论（或失败）已经告诉过主人
}

// TaskStore 是任务账本。
type TaskStore interface {
	Create(ctx context.Context, person, question string) (Task, error)
	Get(ctx context.Context, id int64) (Task, error)
	MarkRunning(ctx context.Context, id int64) error
	// Finish 结束一个还在排队或在查的任务；返回这次有没有结束它（已经结束的不改：取消和查完同时到时，先到的算数）。
	Finish(ctx context.Context, id int64, status Status, conclusion string, sources []string, reason string) (bool, error)
	// Active 是所有人排队中、在查的任务（按派出顺序）。
	Active(ctx context.Context) ([]Task, error)
	// Recent 是某人最近的任务（新的在前）。
	Recent(ctx context.Context, person string, limit int) ([]Task, error)
	// Unreported 是某人已经结束、还没告诉过的任务。
	Unreported(ctx context.Context, person string) ([]Task, error)
	// Board 是某人的任务板：排队中、在查的，加上已经结束、还没告诉过的（按派出顺序）。
	// 一次查询读出来，是同一时刻的快照：同一个任务不会既算在查、又算查完。
	Board(ctx context.Context, person string) ([]Task, error)
	MarkReported(ctx context.Context, ids ...int64) error
	// InterruptActive 把排队中、在查的都标成被中断（服务端启动时调用）；返回条数。
	InterruptActive(ctx context.Context) (int, error)
	Close() error
}

// ErrTaskNotFound 表示没有这个任务。
var ErrTaskNotFound = errors.New("没有这个调查任务")

// OpenTaskStore 打开任务账本：path 是 SQLite 文件路径；空 = 只在内存里（服务端重启就没了）。
func OpenTaskStore(ctx context.Context, path string) (TaskStore, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles, sqliteschema.Options{})
	if err != nil {
		return nil, fmt.Errorf("打开调查任务账本: %w", err)
	}
	return &sqliteTaskStore{db: db}, nil
}

// 任务账本的表结构由迁移文件管理（sqliteschema）。
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

type sqliteTaskStore struct {
	db *sql.DB
}

const taskColumns = `id, person, question, status, conclusion, sources, stop_reason, created_at, finished_at, reported`

func scanTasks(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	var tasks []Task
	for rows.Next() {
		var task Task
		var status, sources string
		var created, finished int64
		var reported int
		if err := rows.Scan(&task.ID, &task.Person, &task.Question, &status, &task.Conclusion, &sources, &task.StopReason,
			&created, &finished, &reported); err != nil {
			return nil, err
		}
		task.Status, task.StatusText = Status(status), Status(status).Label()
		if sources != "" {
			task.Sources = strings.Split(sources, "\n")
		}
		task.CreatedAt = time.Unix(created, 0)
		if finished > 0 {
			task.FinishedAt = time.Unix(finished, 0)
		}
		task.Reported = reported != 0
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *sqliteTaskStore) query(ctx context.Context, where string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskColumns+` FROM research_tasks `+where, args...)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

func (s *sqliteTaskStore) Create(ctx context.Context, person, question string) (Task, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return Task{}, errors.New("要调查的问题不能为空")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO research_tasks (person, question, status, created_at) VALUES (?, ?, ?, ?)`,
		person, question, string(StatusQueued), time.Now().Unix())
	if err != nil {
		return Task{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Task{}, err
	}
	return s.Get(ctx, id)
}

func (s *sqliteTaskStore) Get(ctx context.Context, id int64) (Task, error) {
	tasks, err := s.query(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Task{}, err
	}
	if len(tasks) == 0 {
		return Task{}, ErrTaskNotFound
	}
	return tasks[0], nil
}

func (s *sqliteTaskStore) MarkRunning(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE research_tasks SET status = ? WHERE id = ? AND status = ?`,
		string(StatusRunning), id, string(StatusQueued))
	return err
}

func (s *sqliteTaskStore) Finish(ctx context.Context, id int64, status Status, conclusion string, sources []string, reason string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE research_tasks SET status = ?, conclusion = ?, sources = ?, stop_reason = ?, finished_at = ?
		 WHERE id = ? AND status IN (?, ?)`,
		string(status), conclusion, strings.Join(sources, "\n"), reason, time.Now().Unix(), id,
		string(StatusQueued), string(StatusRunning))
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (s *sqliteTaskStore) Active(ctx context.Context) ([]Task, error) {
	return s.query(ctx, `WHERE status IN (?, ?) ORDER BY id`, string(StatusQueued), string(StatusRunning))
}

func (s *sqliteTaskStore) Recent(ctx context.Context, person string, limit int) ([]Task, error) {
	return s.query(ctx, `WHERE person = ? ORDER BY id DESC LIMIT ?`, person, limit)
}

func (s *sqliteTaskStore) Unreported(ctx context.Context, person string) ([]Task, error) {
	return s.query(ctx, `WHERE person = ? AND reported = 0 AND status NOT IN (?, ?, ?) ORDER BY id`,
		person, string(StatusQueued), string(StatusRunning), string(StatusCancelled))
}

func (s *sqliteTaskStore) Board(ctx context.Context, person string) ([]Task, error) {
	return s.query(ctx, `WHERE person = ? AND (status IN (?, ?) OR (reported = 0 AND status NOT IN (?, ?, ?))) ORDER BY id`,
		person, string(StatusQueued), string(StatusRunning), string(StatusQueued), string(StatusRunning), string(StatusCancelled))
}

func (s *sqliteTaskStore) MarkReported(ctx context.Context, ids ...int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE research_tasks SET reported = 1 WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteTaskStore) InterruptActive(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE research_tasks SET status = ?, stop_reason = ?, finished_at = ? WHERE status IN (?, ?)`,
		string(StatusInterrupted), "服务端重启", time.Now().Unix(), string(StatusQueued), string(StatusRunning))
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	return int(n), err
}

func (s *sqliteTaskStore) Close() error { return s.db.Close() }
