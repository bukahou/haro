// Package notes 是 haro 的记事本存储。
//
// 对外：Open（按连接地址选实现，形同 database/sql.Open）+ Repository（接口）。现在只有 SQLite 实现。
// 找哪一条由模型从列出来的记录里挑（按编号）：这里不做按内容的检索。
package notes

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Note 是一条记录。
type Note struct {
	ID        int64
	Content   string
	CreatedAt time.Time
	DeletedAt time.Time // 没删掉的是零值
}

// Repository 是记事本存储的接口：每种数据库一个实现。
type Repository interface {
	// Save 记一条（内容为空时报错）。
	Save(ctx context.Context, content string) (Note, error)
	// List 返回最近的 limit 条（新的在前）和一共有几条（没删掉的）。
	List(ctx context.Context, limit int) (notes []Note, total int, err error)
	// Deleted 返回最近删掉的 limit 条（最近删的在前）。
	Deleted(ctx context.Context, limit int) ([]Note, error)
	// Delete 软删除一条（数据保留，可以恢复）；不存在或已删除时返回 ErrNotFound。
	Delete(ctx context.Context, id int64) (Note, error)
	// Restore 恢复一条删掉的；不存在或没删除时返回 ErrNotFound。
	Restore(ctx context.Context, id int64) (Note, error)
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
