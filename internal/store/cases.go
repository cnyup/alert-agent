// cases Case 状态持久化（PLAN Wave 4.1）：cases + case_rounds 表。
// CaseState 的唯一写者是 caseflow.Reducer（事务合并）；-replay 不分叉——
// Case 视角是 traces 的投影。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const schemaCases = `
CREATE TABLE IF NOT EXISTS cases (
  id TEXT PRIMARY KEY,                -- 事件 ID 即 Case ID（v1 单事件单 Case）
  fingerprint TEXT NOT NULL,
  state TEXT NOT NULL,                -- investigating|finished|escalated|cancelled
  current_skill TEXT, round INTEGER NOT NULL DEFAULT 0,
  facts TEXT,                         -- JSON []caseflow.Fact
  tokens_used INTEGER NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cases_state ON cases(state);
CREATE TABLE IF NOT EXISTS case_rounds (
  case_id TEXT NOT NULL, round INTEGER NOT NULL, attempt_id TEXT NOT NULL,
  skill TEXT NOT NULL, status TEXT NOT NULL,     -- running|completed|failed|cancelled
  input_state TEXT,                               -- 本轮输入快照（恢复点）
  delta TEXT,                                     -- 校验通过的 Delta JSON
  PRIMARY KEY (case_id, round, attempt_id)
);
`

func (s *Store) ensureCases() error {
	_, err := s.db.Exec(schemaCases)
	return err
}

// CaseRow cases 表行。
type CaseRow struct {
	ID           string
	Fingerprint  string
	State        string // investigating|finished|escalated|cancelled
	CurrentSkill string
	Round        int
	Facts        string // JSON
	TokensUsed   int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UpsertCase Case 建档/更新（幂等：重复事件 ID 原样更新状态字段）。
func (s *Store) UpsertCase(ctx context.Context, c CaseRow) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cases (id, fingerprint, state, current_skill, round, facts, tokens_used, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  state=excluded.state, current_skill=excluded.current_skill,
		  round=excluded.round, facts=excluded.facts,
		  tokens_used=excluded.tokens_used, updated_at=excluded.updated_at`,
		c.ID, c.Fingerprint, c.State, c.CurrentSkill, c.Round, c.Facts,
		c.TokensUsed, c.CreatedAt.UTC(), c.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: case 落库失败: %w", err)
	}
	return nil
}

// GetCase 取单 Case。
func (s *Store) GetCase(ctx context.Context, id string) (*CaseRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, state, current_skill, round, facts, tokens_used, created_at, updated_at
		FROM cases WHERE id=?`, id)
	var c CaseRow
	if err := row.Scan(&c.ID, &c.Fingerprint, &c.State, &c.CurrentSkill, &c.Round,
		&c.Facts, &c.TokensUsed, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("store: case %s 不存在", id)
		}
		return nil, fmt.Errorf("store: case 读取失败: %w", err)
	}
	return &c, nil
}

// RunningRoundAttempts 超时未完成的轮（崩溃恢复扫描）。
func (s *Store) RunningRoundAttempts(ctx context.Context, olderThan time.Time) ([]struct {
	CaseID, AttemptID string
	Round             int
}, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT case_id, round, attempt_id FROM case_rounds
		WHERE status='running'`, olderThan)
	if err != nil {
		return nil, fmt.Errorf("store: running attempt 扫描失败: %w", err)
	}
	defer rows.Close()
	var out []struct {
		CaseID, AttemptID string
		Round             int
	}
	for rows.Next() {
		var r struct {
			CaseID, AttemptID string
			Round             int
		}
		if err := rows.Scan(&r.CaseID, &r.Round, &r.AttemptID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// InsertRoundAttempt 轮开始：attempt 状态 running + 输入快照。
func (s *Store) InsertRoundAttempt(ctx context.Context, caseID string, round int, attemptID, skill, inputState string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO case_rounds (case_id, round, attempt_id, skill, status, input_state)
		VALUES (?,?,?,?, 'running', ?)`,
		caseID, round, attemptID, skill, inputState)
	if err != nil {
		return fmt.Errorf("store: attempt 落库失败: %w", err)
	}
	return nil
}

// CompleteRoundAttempt 轮完成：写 Delta + 状态 completed。
func (s *Store) CompleteRoundAttempt(ctx context.Context, caseID string, round int, attemptID, delta string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE case_rounds SET status='completed', delta=?
		WHERE case_id=? AND round=? AND attempt_id=? AND status='running'`,
		delta, caseID, round, attemptID)
	if err != nil {
		return fmt.Errorf("store: attempt 完成落库失败: %w", err)
	}
	return nil
}

// FailRoundAttempt 轮失败/取消终态。
func (s *Store) FailRoundAttempt(ctx context.Context, caseID string, round int, attemptID, status string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE case_rounds SET status=?
		WHERE case_id=? AND round=? AND attempt_id=? AND status='running'`,
		status, caseID, round, attemptID)
	if err != nil {
		return fmt.Errorf("store: attempt 终态失败: %w", err)
	}
	return nil
}

// RollbackRoundAttempt 崩溃恢复：running attempt 回滚为 failed（重跑走新 attempt）。
func (s *Store) RollbackRoundAttempt(ctx context.Context, caseID string, round int, attemptID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE case_rounds SET status='failed'
		WHERE case_id=? AND round=? AND attempt_id=? AND status='running'`,
		caseID, round, attemptID)
	if err != nil {
		return fmt.Errorf("store: attempt 回滚失败: %w", err)
	}
	return nil
}

// RoundAttemptStatus 查询单 attempt 状态（测试用）。
func (s *Store) RoundAttemptStatus(ctx context.Context, caseID string, round int, attemptID string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `
		SELECT status FROM case_rounds WHERE case_id=? AND round=? AND attempt_id=?`,
		caseID, round, attemptID).Scan(&status)
	return status, err
}
