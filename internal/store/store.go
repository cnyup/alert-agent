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
	st := &Store{db: db}
	if err := st.ensureP1(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: 初始化 P1 schema 失败: %w", err)
	}
	return st, nil
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

// ---- P1：审批状态机与反馈闭环 ----

// Approval 一条 mutating 动作的审批记录（异步跨重启状态机，DESIGN.md §5）。
type Approval struct {
	EventID     string     `json:"event_id"`
	ActionID    string     `json:"action_id"`
	Title       string     `json:"title"`
	Risk        string     `json:"risk"`
	Tool        string     `json:"tool"`
	ArgsJSON    string     `json:"args,omitempty"`
	Status      string     `json:"status"` // pending | approved | rejected | executed | failed
	RequestedAt time.Time  `json:"requested_at"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
	DecidedBy   string     `json:"decided_by,omitempty"` // 飞书 open_id
	Result      string     `json:"result,omitempty"`     // 执行结果
}

// Feedback 人工反馈（认领/误报/根因确认），P2 剧本蒸馏的数据源。
type Feedback struct {
	EventID   string    `json:"event_id"`
	Kind      string    `json:"kind"` // claim | false-positive | root-confirmed
	Operator  string    `json:"operator,omitempty"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

const schemaP1 = `
CREATE TABLE IF NOT EXISTS approvals (
  event_id     TEXT NOT NULL,
  action_id    TEXT NOT NULL,
  title        TEXT NOT NULL,
  risk         TEXT NOT NULL,
  tool         TEXT NOT NULL,
  args         TEXT,
  status       TEXT NOT NULL,
  requested_at TIMESTAMP NOT NULL,
  decided_at   TIMESTAMP,
  decided_by   TEXT,
  result       TEXT,
  PRIMARY KEY (event_id, action_id)
);
CREATE TABLE IF NOT EXISTS card_refs (
  message_id TEXT PRIMARY KEY,
  event_id   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS feedback (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id   TEXT NOT NULL,
  kind       TEXT NOT NULL,
  operator   TEXT,
  note       TEXT,
  created_at TIMESTAMP NOT NULL
);
`

// SaveApproval 写入/覆盖审批记录。
func (s *Store) SaveApproval(ctx context.Context, a Approval) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO approvals
		(event_id, action_id, title, risk, tool, args, status, requested_at, decided_at, decided_by, result)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		a.EventID, a.ActionID, a.Title, a.Risk, a.Tool, a.ArgsJSON, a.Status,
		a.RequestedAt.UTC(), nullableTime(a.DecidedAt), a.DecidedBy, a.Result)
	if err != nil {
		return fmt.Errorf("store: 审批落库失败: %w", err)
	}
	return nil
}

// GetApproval 取单条审批。
func (s *Store) GetApproval(ctx context.Context, eventID, actionID string) (*Approval, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT event_id, action_id, title, risk, tool, args, status, requested_at, decided_at, decided_by, result
		FROM approvals WHERE event_id = ? AND action_id = ?`, eventID, actionID)
	var a Approval
	var decidedAt, args, result sql.NullString
	if err := row.Scan(&a.EventID, &a.ActionID, &a.Title, &a.Risk, &a.Tool, &args, &a.Status,
		&a.RequestedAt, &decidedAt, &a.DecidedBy, &result); err != nil {
		return nil, fmt.Errorf("store: 取回审批失败: %w", err)
	}
	a.ArgsJSON, a.Result = args.String, result.String
	if decidedAt.Valid {
		t, _ := time.Parse("2006-01-02 15:04:05.999999999-07:00", decidedAt.String)
		t2, _ := time.Parse("2006-01-02T15:04:05Z", decidedAt.String)
		if !t.IsZero() {
			a.DecidedAt = &t
		} else if !t2.IsZero() {
			a.DecidedAt = &t2
		}
	}
	return &a, nil
}

// SaveFeedback 反馈落库。
func (s *Store) SaveFeedback(ctx context.Context, f Feedback) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO feedback (event_id, kind, operator, note, created_at) VALUES (?,?,?,?,?)`,
		f.EventID, f.Kind, f.Operator, f.Note, f.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("store: 反馈落库失败: %w", err)
	}
	return nil
}

// Open 追加 P1 schema（幂等）。
func (s *Store) ensureP1() error {
	_, err := s.db.Exec(schemaP1)
	return err
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// ---- P1：卡片消息映射（回复卡片 → 事件关联）----

// SaveCardRef 记录报告卡片的 message_id → event_id。
func (s *Store) SaveCardRef(ctx context.Context, eventID, messageID string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO card_refs (message_id, event_id) VALUES (?,?)`, messageID, eventID); err != nil {
		return fmt.Errorf("store: 卡片映射落库失败: %w", err)
	}
	return nil
}

// EventIDByCard 按卡片 message_id 查事件。
func (s *Store) EventIDByCard(messageID string) (string, bool) {
	var eventID string
	err := s.db.QueryRow(`SELECT event_id FROM card_refs WHERE message_id = ?`, messageID).Scan(&eventID)
	if err != nil {
		return "", false
	}
	return eventID, true
}
