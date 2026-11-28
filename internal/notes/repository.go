// Package notes 是记事与记忆的存储（v10 D2 c，2026-11-22 裁定：记事并进记忆，一张表）。每一条标明记给谁（person：
// 声纹认出的名字，空 = 全家共用，v10 之前记的都算）、来源（主人让记的 / 模型觉得该记的）、要不要每轮记着（常驻）。
// 一个人只看得到、只改得了自己的和全家共用的。
//
// 对外：Open（按连接地址选实现，形同 database/sql.Open）+ Repository（接口）。现在只有 SQLite 实现。
// 按意思检索不在这里：向量另存一张表，检索在 internal/memory。
package notes

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Source 是这一条是怎么来的。
type Source string

const (
	SourceOwner Source = "owner" // 主人让记的（「记一下」「你记住」）
	SourceModel Source = "model" // 模型觉得该记的（主人顺口说的偏好、事实）
)

// Valid 判断是不是认得的来源。
func (s Source) Valid() bool { return s == SourceOwner || s == SourceModel }

// Note 是一条记住的事。
type Note struct {
	ID        int64
	Person    string // 记给谁；空 = 全家共用
	Content   string
	Source    Source
	Resident  bool // 常驻：每轮附上
	CreatedAt time.Time
	DeletedAt time.Time // 没删掉的是零值
}

// NewNote 是要记的一条。
type NewNote struct {
	Person   string // 空 = 全家共用
	Content  string
	Source   Source
	Resident bool
}

// NoteVector 是一条记事的向量（按意思检索用）。
type NoteVector struct {
	ID     int64
	Person string
	Vector []float32
}

// Repository 是记事与记忆存储的接口：每种数据库一个实现。按人的读写都只碰这个人的和全家共用的。
type Repository interface {
	// Save 记一条（内容为空、来源不认得时报错）。
	Save(ctx context.Context, note NewNote) (Note, error)
	// List 返回这个人能看到的最近 limit 条（新的在前）和一共有几条（没删掉的）。
	List(ctx context.Context, person string, limit int) (notes []Note, total int, err error)
	// Deleted 返回这个人能看到的、最近删掉的 limit 条（最近删的在前）。
	Deleted(ctx context.Context, person string, limit int) ([]Note, error)
	// Resident 返回这个人能看到的常驻记事（没删掉的，新的在前），最多 limit 条。
	Resident(ctx context.Context, person string, limit int) ([]Note, error)
	// Get 按编号取这个人能看到的几条（删掉的也给，按 ids 的顺序）；没有的编号不出现。
	Get(ctx context.Context, person string, ids []int64) ([]Note, error)
	// Delete 软删除一条（数据保留，可以恢复）；不存在、看不到或已删除时返回 ErrNotFound。
	Delete(ctx context.Context, person string, id int64) (Note, error)
	// Restore 恢复一条删掉的；不存在、看不到或没删除时返回 ErrNotFound。
	Restore(ctx context.Context, person string, id int64) (Note, error)
	// Unindexed 返回还没有这个模型的向量的记事（没删掉的，旧的在前），最多 limit 条。
	Unindexed(ctx context.Context, model string, limit int) ([]Note, error)
	// SaveVector 存一条记事的向量（同一条再存就替换：换了模型时）。
	SaveVector(ctx context.Context, id int64, model string, vector []float32) error
	// Vectors 返回没删掉的记事里这个模型的全部向量。
	Vectors(ctx context.Context, model string) ([]NoteVector, error)
	// Vector 返回一条记事这个模型的向量（删掉的也给：恢复时放回索引）；没有时为 nil。
	Vector(ctx context.Context, id int64, model string) ([]float32, error)
	// ErasePerson 删掉这个人记的全部（含删掉的、向量），彻底删账号时用。
	ErasePerson(ctx context.Context, person string) error
	Close() error
}

// ErrNotFound 表示要删除（恢复）的记录不存在，或者已经删过（没删过）。
var ErrNotFound = errors.New("没有这条记录")

// Open 按连接地址打开记事本存储：文件路径（或 sqlite:路径）→ SQLite。
func Open(ctx context.Context, location string) (Repository, error) {
	if location == "" {
		return nil, errors.New("没有指定记事本存储位置")
	}
	return OpenSQLite(ctx, strings.TrimPrefix(location, "sqlite:"))
}
