package notes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
)

// unixTimeMigration（迁移 2）：时间从带时区的 RFC 3339 文本改存 Unix 秒（UTC）。按文本排序时，
// 不同时区写下的记录先后会排错（"10:00+09:00" 排在 "02:00Z" 后面，其实早一个小时）。
// 用 Go 写：每条都按以前读出时的格式（time.RFC3339）解析，有一条解析不了就整个迁移失败（事务回滚、库保持原样），
// 不写成 0、不丢记录；编号原样保留，新记录接着排。
var unixTimeMigration = goose.NewGoMigration(2, &goose.GoFunc{RunTx: storeTimesAsUnixSeconds}, nil)

// textTimeNote 是迁移前的一行：时间是文本。
type textTimeNote struct {
	id        int64
	content   string
	createdAt string
	deletedAt sql.NullString
}

func storeTimesAsUnixSeconds(ctx context.Context, tx *sql.Tx) error {
	var sequence sql.NullInt64 // 用过的最大编号（AUTOINCREMENT 不重用编号，删表会把它一起删掉）
	err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'notes'`).Scan(&sequence)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, content, created_at, deleted_at FROM notes ORDER BY id`)
	if err != nil {
		return err
	}
	var existing []textTimeNote
	for rows.Next() {
		var note textTimeNote
		if err := rows.Scan(&note.id, &note.content, &note.createdAt, &note.deletedAt); err != nil {
			rows.Close()
			return err
		}
		existing = append(existing, note)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE notes_unix_time (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		content    TEXT    NOT NULL,
		created_at INTEGER NOT NULL, -- Unix 秒（UTC）
		deleted_at INTEGER           -- Unix 秒（UTC）；NULL = 没删
	)`); err != nil {
		return err
	}
	for _, note := range existing {
		created, err := time.Parse(time.RFC3339, note.createdAt)
		if err != nil {
			return fmt.Errorf("记录 %d 的时间 %q 转换不了: %w", note.id, note.createdAt, err)
		}
		var deleted sql.NullInt64
		if note.deletedAt.Valid {
			at, err := time.Parse(time.RFC3339, note.deletedAt.String)
			if err != nil {
				return fmt.Errorf("记录 %d 的删除时间 %q 转换不了: %w", note.id, note.deletedAt.String, err)
			}
			deleted = sql.NullInt64{Int64: at.Unix(), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes_unix_time (id, content, created_at, deleted_at) VALUES (?, ?, ?, ?)`,
			note.id, note.content, created.Unix(), deleted); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`DROP TABLE notes`, // 连同旧的索引
		`ALTER TABLE notes_unix_time RENAME TO notes`,
		`CREATE INDEX notes_created_at ON notes (created_at)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if !sequence.Valid {
		return nil
	}
	// 用过的最大编号接上：最后几条要是被彻底删过，新记录也不重用它们的编号
	if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = MAX(seq, ?) WHERE name = 'notes'`, sequence.Int64); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) SELECT 'notes', ? WHERE NOT EXISTS
		(SELECT 1 FROM sqlite_sequence WHERE name = 'notes')`, sequence.Int64)
	return err
}
