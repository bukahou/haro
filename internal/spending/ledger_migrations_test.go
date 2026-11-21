package spending

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// 改用迁移之前的两种旧账本（加缓存记账之前的、之后的），迁移后：原来的记录都在、编号不变，
// 新记录的编号接着往下排；不完整的调用不知道的 token 记成 NULL。再打开一次不会重复迁移。
func TestLedgerMigratesOldLayouts(t *testing.T) {
	for _, c := range []struct{ name, schema string }{
		{"加缓存记账之前", `CREATE TABLE charges (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, purpose TEXT NOT NULL,
			item TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			credits INTEGER NOT NULL DEFAULT 0, yen REAL NOT NULL DEFAULT 0);
			INSERT INTO charges (at, purpose, item, input_tokens, output_tokens, yen) VALUES (1, 'chat', 'llm:a', 100, 10, 1.5);
			INSERT INTO charges (at, purpose, item, input_tokens, output_tokens, yen) VALUES (2, 'chat', 'llm:a', 200, 20, 2.5);`},
		{"加缓存记账之后、改用迁移之前", `CREATE TABLE charges (id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, purpose TEXT NOT NULL,
			item TEXT NOT NULL, input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0, cache_write_tokens INTEGER NOT NULL DEFAULT 0,
			credits INTEGER NOT NULL DEFAULT 0, yen REAL NOT NULL DEFAULT 0);
			INSERT INTO charges (at, purpose, item, input_tokens, output_tokens, cache_read_tokens, yen) VALUES (1, 'chat', 'llm:a', 100, 10, 50, 1.5);
			INSERT INTO charges (at, purpose, item, input_tokens, output_tokens, cache_read_tokens, yen) VALUES (2, 'chat', 'llm:a', 200, 20, 80, 2.5);`},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "usage.db")
			old, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := old.ExecContext(ctx, c.schema); err != nil {
				t.Fatal(err)
			}
			old.Close()

			ledger, err := OpenLedger(ctx, path)
			if err != nil {
				t.Fatalf("迁移旧账本：%v", err)
			}
			if totals, err := ledger.Total(ctx, time.Unix(0, 0)); err != nil || totals.Yen != 4 {
				t.Errorf("原来的记录应都在：%+v %v", totals, err)
			}
			if err := ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:a", InputTokens: 300, Incomplete: true, Yen: 0.5}); err != nil {
				t.Fatal(err)
			}
			if err := ledger.Record(ctx, Charge{Purpose: PurposeChat, Item: "llm:a", TokensUnknown: true, Incomplete: true}); err != nil {
				t.Fatal(err)
			}
			ledger.Close()

			ledger, err = OpenLedger(ctx, path) // 再打开：已经迁移过，不重复
			if err != nil {
				t.Fatalf("再打开：%v", err)
			}
			defer ledger.Close()
			if err := ledger.RaiseCap(ctx, CapRaise{Yen: 50, Person: "主人"}); err != nil {
				t.Fatalf("迁移后的旧账本也要能记临时提高的上限：%v", err)
			}
			if raised, err := ledger.CapRaised(ctx, time.Unix(0, 0)); err != nil || raised != 50 {
				t.Errorf("提高的上限：%v %v", raised, err)
			}
			db := ledger.(*sqliteLedger).db
			type row struct {
				id                    int64
				input, output, cached sql.NullInt64
				incomplete            bool
			}
			rows, err := db.QueryContext(ctx, `SELECT id, input_tokens, output_tokens, cache_read_tokens, incomplete FROM charges ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			var got []row
			for rows.Next() {
				var r row
				if err := rows.Scan(&r.id, &r.input, &r.output, &r.cached, &r.incomplete); err != nil {
					t.Fatal(err)
				}
				got = append(got, r)
			}
			rows.Close()
			if len(got) != 4 || got[0].id != 1 || got[1].id != 2 || got[2].id != 3 || got[3].id != 4 {
				t.Fatalf("编号应不变、新记录接着排：%+v", got)
			}
			if !got[0].input.Valid || got[0].input.Int64 != 100 || got[0].incomplete {
				t.Errorf("旧记录应原样保留：%+v", got[0])
			}
			if !got[2].input.Valid || got[2].input.Int64 != 300 || got[2].output.Valid || !got[2].incomplete {
				t.Errorf("不完整的调用：输入照实记、输出记 NULL：%+v", got[2])
			}
			if got[3].input.Valid || got[3].output.Valid || got[3].cached.Valid || !got[3].incomplete {
				t.Errorf("什么都不知道的调用：各项 token 记 NULL：%+v", got[3])
			}
		})
	}
}
