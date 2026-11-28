package notes

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/bukahou/haro/internal/embedding"
	"github.com/bukahou/haro/internal/sqliteschema"
)

// SQLiteStore 是 Repository 的 SQLite 实现。时间按 UTC 的 Unix 秒存（迁移 2），给人看时才换成本地时间；
// 记给谁、来源、常驻和向量表是迁移 3（记事并进记忆）。
type SQLiteStore struct {
	db *sql.DB
}

var _ Repository = (*SQLiteStore)(nil)

// 记事库的表结构由迁移文件管理（sqliteschema）；迁移 2 要逐条转换时间，用 Go 写（notes_migrations.go）。
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// OpenSQLite 打开（必要时创建目录和文件）SQLite 数据库，并把表结构升到最新。
func OpenSQLite(ctx context.Context, path string) (*SQLiteStore, error) {
	db, err := sqliteschema.Open(ctx, path, migrationFiles,
		sqliteschema.Options{Migrate: []goose.ProviderOption{goose.WithGoMigrations(unixTimeMigration)}})
	if err != nil {
		return nil, fmt.Errorf("打开记事本数据库 %s: %w", path, err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Save 记一条。
func (s *SQLiteStore) Save(ctx context.Context, note NewNote) (Note, error) {
	content := strings.TrimSpace(note.Content)
	switch {
	case content == "":
		return Note{}, errors.New("内容为空")
	case !note.Source.Valid():
		return Note{}, fmt.Errorf("不认得的来源 %q", note.Source)
	}
	created := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `INSERT INTO notes (content, created_at, person, source, resident) VALUES (?, ?, ?, ?, ?)`,
		content, created, note.Person, string(note.Source), note.Resident)
	if err != nil {
		return Note{}, err
	}
	id, err := result.LastInsertId()
	return Note{ID: id, Person: note.Person, Content: content, Source: note.Source, Resident: note.Resident,
		CreatedAt: time.Unix(created, 0).UTC()}, err
}

// noteColumns 是读一条记事的列（query 按这个顺序读）。
const noteColumns = `id, content, created_at, deleted_at, person, source, resident`

// visible 是「这个人能看到的」条件：自己的和全家共用的（参数：person）。
const visible = `person IN (?, '')`

// List 返回这个人能看到的最近 limit 条（没删掉的，新的在前）和一共有几条。
func (s *SQLiteStore) List(ctx context.Context, person string, limit int) ([]Note, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes WHERE deleted_at IS NULL AND `+visible, person).
		Scan(&total); err != nil {
		return nil, 0, err
	}
	notes, err := s.query(ctx, `SELECT `+noteColumns+` FROM notes WHERE deleted_at IS NULL AND `+visible+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, person, limit)
	return notes, total, err
}

// Deleted 返回这个人能看到的、最近删掉的 limit 条。
func (s *SQLiteStore) Deleted(ctx context.Context, person string, limit int) ([]Note, error) {
	return s.query(ctx, `SELECT `+noteColumns+` FROM notes WHERE deleted_at IS NOT NULL AND `+visible+`
		ORDER BY deleted_at DESC, id DESC LIMIT ?`, person, limit)
}

// Resident 返回这个人能看到的常驻记事（没删掉的，新的在前），最多 limit 条。
func (s *SQLiteStore) Resident(ctx context.Context, person string, limit int) ([]Note, error) {
	return s.query(ctx, `SELECT `+noteColumns+` FROM notes WHERE deleted_at IS NULL AND resident = 1 AND `+visible+`
		ORDER BY created_at DESC, id DESC LIMIT ?`, person, limit)
}

// Get 按编号取这个人能看到的几条（删掉的也给），按 ids 的顺序；没有的编号不出现。
func (s *SQLiteStore) Get(ctx context.Context, person string, ids []int64) ([]Note, error) {
	byID := map[int64]Note{}
	for _, id := range ids {
		found, err := s.query(ctx, `SELECT `+noteColumns+` FROM notes WHERE id = ? AND `+visible, id, person)
		if err != nil {
			return nil, err
		}
		for _, note := range found {
			byID[note.ID] = note
		}
	}
	var notes []Note
	for _, id := range ids {
		if note, ok := byID[id]; ok {
			notes = append(notes, note)
			delete(byID, id)
		}
	}
	return notes, nil
}

// Delete 软删除一条：只打标记，数据还在，可以恢复。
func (s *SQLiteStore) Delete(ctx context.Context, person string, id int64) (Note, error) {
	return s.mark(ctx, person, id, `deleted_at IS NULL`, sql.NullInt64{Int64: time.Now().Unix(), Valid: true})
}

// Restore 恢复一条删掉的。
func (s *SQLiteStore) Restore(ctx context.Context, person string, id int64) (Note, error) {
	return s.mark(ctx, person, id, `deleted_at IS NOT NULL`, sql.NullInt64{})
}

// mark 把这个人能看到的、满足 state 的那一条的 deleted_at 改成 deletedAt，交回改完的那一条；没有这样的一条时返回 ErrNotFound。
// 改和读回是同一条语句（RETURNING）：出错就是出错（交回原来的错误），不会把「已经改了、读回时出错」说成「没有这一条」。
func (s *SQLiteStore) mark(ctx context.Context, person string, id int64, state string, deletedAt sql.NullInt64) (Note, error) {
	notes, err := s.query(ctx, `UPDATE notes SET deleted_at = ? WHERE id = ? AND `+visible+` AND `+state+
		` RETURNING `+noteColumns, deletedAt, id, person)
	switch {
	case err != nil:
		return Note{}, err
	case len(notes) == 0:
		return Note{}, ErrNotFound
	}
	return notes[0], nil
}

// Unindexed 返回还没有这个模型的向量的记事（没删掉的，旧的在前），最多 limit 条。
func (s *SQLiteStore) Unindexed(ctx context.Context, model string, limit int) ([]Note, error) {
	return s.query(ctx, `SELECT `+noteColumns+` FROM notes WHERE deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM note_vectors v WHERE v.note_id = notes.id AND v.model = ?)
		ORDER BY id LIMIT ?`, model, limit)
}

// SaveVector 存一条记事的向量（同一条再存就替换）。
func (s *SQLiteStore) SaveVector(ctx context.Context, id int64, model string, vector []float32) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO note_vectors (note_id, model, vector) VALUES (?, ?, ?)
		ON CONFLICT (note_id) DO UPDATE SET model = excluded.model, vector = excluded.vector`, id, model, embedding.EncodeVector(vector))
	return err
}

// Vectors 返回没删掉的记事里这个模型的全部向量。
func (s *SQLiteStore) Vectors(ctx context.Context, model string) ([]NoteVector, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.person, v.vector FROM notes n JOIN note_vectors v ON v.note_id = n.id
		WHERE n.deleted_at IS NULL AND v.model = ? ORDER BY n.id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var vectors []NoteVector
	for rows.Next() {
		var item NoteVector
		var data []byte
		if err := rows.Scan(&item.ID, &item.Person, &data); err != nil {
			return nil, err
		}
		if item.Vector, err = embedding.DecodeVector(data); err != nil {
			return nil, fmt.Errorf("记事 %d 的向量: %w", item.ID, err)
		}
		vectors = append(vectors, item)
	}
	return vectors, rows.Err()
}

// Vector 返回一条记事这个模型的向量（删掉的也给）；没有时为 nil。
func (s *SQLiteStore) Vector(ctx context.Context, id int64, model string) ([]float32, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT vector FROM note_vectors WHERE note_id = ? AND model = ?`, id, model).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return embedding.DecodeVector(data)
}

func (s *SQLiteStore) query(ctx context.Context, sqlText string, args ...any) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []Note
	for rows.Next() {
		var note Note
		var created int64
		var deleted sql.NullInt64
		var source string
		if err := rows.Scan(&note.ID, &note.Content, &created, &deleted, &note.Person, &source, &note.Resident); err != nil {
			return nil, err
		}
		note.Source = Source(source)
		note.CreatedAt = time.Unix(created, 0).UTC()
		if deleted.Valid {
			note.DeletedAt = time.Unix(deleted.Int64, 0).UTC()
		}
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

// ErasePerson 实现 Repository。
func (s *SQLiteStore) ErasePerson(ctx context.Context, person string) error {
	return sqliteschema.ErasePerson(ctx, s.db, person,
		`DELETE FROM note_vectors WHERE note_id IN (SELECT id FROM notes WHERE person = ?)`,
		`DELETE FROM notes WHERE person = ?`)
}
