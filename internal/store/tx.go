// tx 事务支持（PLAN-CASE-SUPERVISOR Wave 1.1）：database/sql 原生
// Begin/Commit/Rollback。SQLite 单写者——事务内严禁任何外部 IO
// （LLM/通知调用不得进事务）。
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Tx 事务句柄：与 Store 同 API 面（写操作），生命周期由 WithTx 管理。
type Tx struct {
	tx *sql.Tx
}

// WithTx 在事务中执行 fn：fn 返回 nil 提交，返回 err 回滚。
// fn 内不得做外部 IO（单写者锁会阻塞其他 worker 的写）。
func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	if err := fn(&Tx{tx: tx}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("store: 事务回滚失败（原错误 %v）: %w", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: 事务提交失败: %w", err)
	}
	return nil
}

// Exec 在事务内执行写语句。
func (t *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryRow 在事务内查询单行。
func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// Query 在事务内查询多行。
func (t *Tx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}
