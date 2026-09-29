// jobs 持久队列（PLAN-CASE-SUPERVISOR Wave 1）：SQLite 落库即持久，
// channel 仅作唤醒信号。崩溃不丢任务；stale running 由回收器重新抢占。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const schemaJobs = `
CREATE TABLE IF NOT EXISTS jobs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,                 -- webhook_event|feishu_msg|approval_exec|case_round
  dedup_key TEXT UNIQUE,              -- 入队幂等（fingerprint/msg_id/attempt_id）
  payload TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',   -- pending|running|done|failed
  attempts INTEGER NOT NULL DEFAULT 0,
  available_at TIMESTAMP NOT NULL,    -- 退避重试
  claimed_by TEXT, claimed_at TIMESTAMP,
  created_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_ready ON jobs(status, available_at);
`

// ensureJobs 追加 jobs schema（幂等）。
func (s *Store) ensureJobs() error {
	_, err := s.db.Exec(schemaJobs)
	return err
}

// Job 队列条目。
type Job struct {
	ID          int64
	Kind        string
	DedupKey    string
	Payload     string
	Status      string
	Attempts    int
	AvailableAt time.Time
	ClaimedBy   string
	ClaimedAt   *time.Time
	CreatedAt   time.Time
}

// EnqueueJob 入队（幂等：dedupKey 冲突时已有行原样保留）。
func (s *Store) EnqueueJob(ctx context.Context, kind, payload, dedupKey string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (kind, dedup_key, payload, status, available_at, created_at)
		VALUES (?, ?, ?, 'pending', ?, ?)
		ON CONFLICT(dedup_key) DO NOTHING`,
		kind, dedupKey, payload, time.Now().UTC(), time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("store: 入队失败: %w", err)
	}
	return res.LastInsertId()
}

// ClaimNextJob 抢占一条就绪 job：候选子查询 + 条件 UPDATE（WAL 下恰好一个
// worker 成功，机制已探针验证 F2）。无可领返回 (nil, nil)。kinds 空则不限。
func (s *Store) ClaimNextJob(ctx context.Context, workerID string, kinds []string) (*Job, error) {
	now := time.Now().UTC()
	// 空 kinds = 不限 kind（无过滤子句）；否则 IN (json_each(?))
	var subArgs []any
	kindFilter := ""
	if len(kinds) > 0 {
		b, err := json.Marshal(kinds)
		if err != nil {
			return nil, fmt.Errorf("store: kinds 序列化失败: %w", err)
		}
		kindFilter = ` AND kind IN (SELECT value FROM json_each(?))`
		subArgs = append(subArgs, string(b))
	}
	// 单语句条件 UPDATE：子查询选候选，外层 WHERE status='pending' 保证原子性
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status='running', claimed_by=?, claimed_at=?, attempts=attempts+1
		WHERE id = (
			SELECT id FROM jobs
			WHERE status='pending' AND available_at <= ?`+kindFilter+`
			ORDER BY id LIMIT 1
		) AND status='pending'`,
		append([]any{workerID, now, now}, subArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("store: claim 抢占失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: claim 读取受影响行失败: %w", err)
	}
	if n != 1 {
		return nil, nil
	}
	// 回读完整 job（需要 payload 与 id）
	row := s.db.QueryRowContext(ctx, `
		SELECT id, kind, dedup_key, payload, status, attempts, available_at, claimed_by, claimed_at, created_at
		FROM jobs WHERE claimed_by=? AND status='running' AND claimed_at=?`,
		workerID, now)
	var (
		j         Job
		dedup     sql.NullString
		claimedBy sql.NullString
		claimedAt sql.NullTime
	)
	if err := row.Scan(&j.ID, &j.Kind, &dedup, &j.Payload, &j.Status, &j.Attempts,
		&j.AvailableAt, &claimedBy, &claimedAt, &j.CreatedAt); err != nil {
		return nil, fmt.Errorf("store: claim 回读失败: %w", err)
	}
	j.DedupKey, j.ClaimedBy = dedup.String, claimedBy.String
	if claimedAt.Valid {
		t := claimedAt.Time
		j.ClaimedAt = &t
	}
	return &j, nil
}

// ReclaimStaleJobs 崩溃恢复：抢占 claimed_at 早于 cutoff 的 running job。
// attempts 已达 maxAttempts 的置 failed（不再重试）；其余置回 pending。
// 返回重置为 pending 的 job ID 列表。
func (s *Store) ReclaimStaleJobs(ctx context.Context, olderThan time.Duration, maxAttempts int) ([]int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	// 1) 超限 → failed
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status='failed'
		WHERE status='running' AND claimed_at < ? AND attempts >= ?`, cutoff, maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("store: stale 超限置 failed 失败: %w", err)
	}
	// 2) 未超限 → 置回 pending（保留 attempts 计数）
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status='pending', claimed_by=NULL, claimed_at=NULL
		WHERE status='running' AND claimed_at < ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: stale 回收失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("store: stale 回收读取受影响行失败: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	// 3) 返回刚回收的 job（claim 过至少一次 = attempts>0 的 pending）
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM jobs WHERE status='pending' AND attempts > 0 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: stale 回收查询失败: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// FinishJob job 终态落库（done/failed）。仅 running 可推进：防止原 worker
// 崩溃后回收重跑期间，迟到的 FinishJob 覆盖新状态（Wave 1.3 测试固化）。
func (s *Store) FinishJob(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status=? WHERE id=? AND status='running'`, status, id)
	if err != nil {
		return fmt.Errorf("store: job 终态失败: %w", err)
	}
	return nil
}

// QueueDepth 当前 pending 积压（监控/503 判定用）。
func (s *Store) QueueDepth(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status='pending'`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 队列深度查询失败: %w", err)
	}
	return n, nil
}
