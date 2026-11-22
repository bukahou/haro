package research

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pressly/goose/v3"
)

// sourcesAsJSONMigration（迁移 2）：调查的来源从「换行分隔的一段文本」改存 JSON 数组。来源是模型交的字符串，
// 里面可以带换行，按换行切回来会切成两条。用 Go 写：旧的每一行按以前的规则（换行分隔，空 = 没有）切开再编成 JSON；
// 列换成默认 '[]' 的新列（SQLite 改不了已有列的默认值：加新列、搬过去、删旧列、改名）。
var sourcesAsJSONMigration = goose.NewGoMigration(2, &goose.GoFunc{RunTx: storeSourcesAsJSON}, nil)

// taskMigrations 是任务账本的 SQL 迁移之外、用 Go 写的那几步（交给 sqliteschema.Open）。
var taskMigrations = []goose.ProviderOption{goose.WithGoMigrations(sourcesAsJSONMigration)}

func storeSourcesAsJSON(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE research_tasks ADD COLUMN source_list TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, sources FROM research_tasks WHERE sources <> ''`)
	if err != nil {
		return err
	}
	joined := map[int64]string{}
	for rows.Next() {
		var id int64
		var sources string
		if err := rows.Scan(&id, &sources); err != nil {
			rows.Close()
			return err
		}
		joined[id] = sources
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for id, sources := range joined {
		encoded, err := json.Marshal(strings.Split(sources, "\n"))
		if err != nil {
			return fmt.Errorf("调查 %d 的来源编不成 JSON: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE research_tasks SET source_list = ? WHERE id = ?`, string(encoded), id); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`ALTER TABLE research_tasks DROP COLUMN sources`,
		`ALTER TABLE research_tasks RENAME COLUMN source_list TO sources`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}
