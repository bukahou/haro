// Package notes 是 haro 的记事本存储（v2.5）。
//
// 对外：Open（Facade，按连接地址选择实现，形同 database/sql.Open）+ Repository（接口）。
// 现在只有 SQLite 实现（开发阶段：数据就在项目的 data/ 下，随时可以清理）；
// 以后可以新增 PostgreSQL / MySQL 实现，改配置即可切换，调用方（大脑里的记事本工具，v5 起内置）不用改。
package notes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Note 是一条记录。
type Note struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Repository 是记事本存储的接口：每种数据库一个实现。
type Repository interface {
	// Save 记一条（内容为空时报错）。
	Save(ctx context.Context, content string) (Note, error)
	// Search 按内容相关度查找，返回最相关的 limit 条；一条都不相关时返回空。
	Search(ctx context.Context, query string, limit int) ([]Note, error)
	// Recent 返回最近 limit 条。
	Recent(ctx context.Context, limit int) ([]Note, error)
	// Delete 软删除一条（数据保留，可找回）；不存在或已删除时返回 ErrNotFound。
	Delete(ctx context.Context, id int64) (Note, error)
	Close() error
}

// ErrNotFound 表示要删除的记录不存在（或已经删过）。
var ErrNotFound = errors.New("没有这条记录")

// Open 按连接地址打开记事本存储：
//   - 文件路径（或 sqlite:路径）→ SQLite
//   - postgres:// / mysql:// → 以后实现
func Open(ctx context.Context, location string) (Repository, error) {
	switch {
	case strings.HasPrefix(location, "postgres://"), strings.HasPrefix(location, "postgresql://"),
		strings.HasPrefix(location, "mysql://"):
		return nil, fmt.Errorf("暂不支持 %s（现在只有 SQLite）", strings.SplitN(location, "://", 2)[0])
	case location == "":
		return nil, errors.New("没有指定记事本存储位置")
	default:
		return OpenSQLite(ctx, strings.TrimPrefix(location, "sqlite:"))
	}
}
