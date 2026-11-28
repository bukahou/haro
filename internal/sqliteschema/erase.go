package sqliteschema

import (
	"context"
	"database/sql"
	"fmt"
)

// ErasePerson 在一个事务里依次执行 statements（每条都只带一个参数：person），删掉一个人的数据（彻底删账号时用）。
// 顺序由调用方排（先删引用别的表的行）。
func ErasePerson(ctx context.Context, db *sql.DB, person string, statements ...string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement, person); err != nil {
			return fmt.Errorf("删一个人的数据（%s）: %w", statement, err)
		}
	}
	return tx.Commit()
}
