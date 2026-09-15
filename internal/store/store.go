// Package store 持久化层：SQLite 起步（modernc.org/sqlite 纯 Go 无 CGO），
// 表 events / traces。回放（replay）与反馈闭环都建立在这层之上。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/cnyup/alert-agent/pkg/model"
)

// TraceEntry 全链路 trace 的单条记录：管道 stage、agent 每步 IO 统一此结构。
// ID 用 "T1"/"S1" 式编号，报告证据链按 ID 引用。
type TraceEntry struct {
	Seq    int       `json:"seq"`
	ID     string    `json:"id"`
	Kind   string    `json:"kind"` // stage | tool | model | notify
	Name   string    `json:"name"`
	Input  string    `json:"input,omitempty"`
	Output string    `json:"output,omitempty"`
	Err    string    `json:"err,omitempty"`
	At     time.Time `json:"at"`
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  id          TEXT PRIMARY KEY,
  fingerprint TEXT NOT NULL,
  source      TEXT NOT NULL,
  severity    TEXT NOT NULL,
  title       TEXT NOT NULL,
  description TEXT,
  labels      TEXT NOT NULL,
  raw         TEXT,
  refs        TEXT,
  occurred_at TIMESTAMP NOT NULL,
  received_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_fingerprint ON events(fingerprint);
CREATE INDEX IF NOT EXISTS idx_events_received ON events(received_at DESC);
CREATE TABLE IF NOT EXISTS traces (
  event_id TEXT NOT NULL,
  seq      INTEGER NOT NULL,
  entry    TEXT NOT NULL,
  PRIMARY KEY (event_id, seq)
);
`

// Open 打开（必要时创建）数据库。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("store: 打开 %s 失败: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 初始化 schema 失败: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭连接。
func (s *Store) Close() error { return s.db.Close() }

// SaveEvent 事件落库（幂等：重复 ID 忽略）。
func (s *Store) SaveEvent(ctx context.Context, evt *model.AlertEvent) error {
	labels, _ := json.Marshal(evt.Labels)
	refs, _ := json.Marshal(evt.Refs)
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO events
		(id, fingerprint, source, severity, title, description, labels, raw, refs, occurred_at, received_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		evt.ID, evt.Fingerprint, evt.Source, string(evt.Severity), evt.Title, evt.Description,
		string(labels), string(evt.Raw), string(refs), evt.OccurredAt.UTC(), evt.ReceivedAt.UTC())
	if err != nil {
		return fmt.Errorf("store: 事件落库失败: %w", err)
	}
	return nil
}

// GetEvent 按 ID 取回事件。
func (s *Store) GetEvent(ctx context.Context, id string) (*model.AlertEvent, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, source, severity, title, description, labels, raw, refs, occurred_at, received_at
		FROM events WHERE id = ?`, id)
	var (
		evt      model.AlertEvent
		labels   string
		refs     sql.NullString
		raw      sql.NullString
		desc     sql.NullString
	)
	if err := row.Scan(&evt.ID, &evt.Fingerprint, &evt.Source, &evt.Severity, &evt.Title, &desc,
		&labels, &raw, &refs, &evt.OccurredAt, &evt.ReceivedAt); err != nil {
		return nil, fmt.Errorf("store: 取回事件 %s 失败: %w", id, err)
	}
	evt.Description = desc.String
	evt.Raw = json.RawMessage(raw.String)
	if refs.Valid && refs.String != "" {
		_ = json.Unmarshal([]byte(refs.String), &evt.Refs)
	}
	if labels != "" {
		_ = json.Unmarshal([]byte(labels), &evt.Labels)
	}
	return &evt, nil
}

// AppendTrace 追加一条 trace 记录（Seq 由调用方编排，通常递增计数器）。
func (s *Store) AppendTrace(ctx context.Context, eventID string, e TraceEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("store: trace 编码失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO traces (event_id, seq, entry) VALUES (?,?,?)`,
		eventID, e.Seq, string(b)); err != nil {
		return fmt.Errorf("store: trace 落库失败: %w", err)
	}
	return nil
}

// Trace 取回某事件的完整 trace（按 Seq 升序）——replay 的数据源。
func (s *Store) Trace(ctx context.Context, eventID string) ([]TraceEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entry FROM traces WHERE event_id = ? ORDER BY seq`, eventID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 trace 失败: %w", err)
	}
	defer rows.Close()
	var out []TraceEntry
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var e TraceEntry
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			return nil, fmt.Errorf("store: trace 解码失败: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
