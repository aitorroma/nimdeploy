package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// hubStore keeps the hub's data in libSQL: agents (with their tokens), every
// event they sent, and the current state of each deploy for the dashboard.

const hubSchemaVersion = 1

var hubSchema = []string{
	`CREATE TABLE IF NOT EXISTS hub_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS agents (
		name        TEXT PRIMARY KEY,
		token       TEXT NOT NULL,
		created_at  TEXT NOT NULL,
		revoked_at  TEXT,
		host        TEXT,
		version     TEXT,
		last_seen   TEXT,
		started_at  TEXT,
		heartbeat_s INTEGER,
		outbox      INTEGER,
		labels      TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id          TEXT PRIMARY KEY,
		agent       TEXT NOT NULL,
		type        TEXT NOT NULL,
		time        TEXT NOT NULL,
		deploy      TEXT,
		status      TEXT,
		trigger     TEXT,
		commit_sha  TEXT,
		ref         TEXT,
		pusher      TEXT,
		duration    TEXT,
		error       TEXT,
		labels      TEXT,
		state       TEXT,
		log_tail    TEXT,
		received_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS events_time ON events (time)`,
	`CREATE INDEX IF NOT EXISTS events_deploy ON events (agent, deploy, time)`,
	`CREATE TABLE IF NOT EXISTS deploys (
		agent       TEXT NOT NULL,
		deploy      TEXT NOT NULL,
		provider    TEXT,
		repository  TEXT,
		branch      TEXT,
		schedule    TEXT,
		labels      TEXT,
		state       TEXT,
		status      TEXT,
		updated_at  TEXT NOT NULL,
		in_inventory INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (agent, deploy)
	)`,
}

type hubStore struct {
	db *libsqlClient
}

func (s *hubStore) migrate(ctx context.Context) error {
	for _, q := range hubSchema {
		if err := s.db.Exec(ctx, sqlStmt{SQL: q}); err != nil {
			return err
		}
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO hub_meta (key, value) VALUES ('schema', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, Args: []any{fmt.Sprint(hubSchemaVersion)}})
}

func jsonText(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return string(b)
}

func nowText() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// --- agents --------------------------------------------------------------------------

type hubAgentRow struct {
	Name, Host, Version  string
	CreatedAt, RevokedAt string
	LastSeen, StartedAt  time.Time
	HeartbeatS, Outbox   int64
	Labels               map[string]string
	token                string
}

func (a hubAgentRow) online(now time.Time) bool {
	if a.LastSeen.IsZero() {
		return false
	}
	every := time.Duration(a.HeartbeatS) * time.Second
	if every <= 0 {
		every = defaultHeartbeat
	}
	return now.Sub(a.LastSeen) <= 3*every+30*time.Second
}

func newAgentToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// addAgent creates an agent, or gives an existing one a new token.
func (s *hubStore) addAgent(ctx context.Context, name string) (string, error) {
	if !agentNameRe.MatchString(name) {
		return "", fmt.Errorf("agent name %q: letters, digits, _ . - (max 64)", name)
	}
	token := newAgentToken()
	err := s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO agents (name, token, created_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET token = excluded.token, revoked_at = NULL`, Args: []any{name, token, nowText()}})
	return token, err
}

func (s *hubStore) revokeAgent(ctx context.Context, name string) error {
	rows, err := s.db.Query(ctx, `SELECT name FROM agents WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("unknown agent %q", name)
	}
	return s.db.Exec(ctx, sqlStmt{SQL: `UPDATE agents SET revoked_at = ? WHERE name = ?`, Args: []any{nowText(), name}})
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func agentFromRow(r sqlRow) hubAgentRow {
	a := hubAgentRow{Name: r.str("name"), Host: r.str("host"), Version: r.str("version"), CreatedAt: r.str("created_at"),
		RevokedAt: r.str("revoked_at"), LastSeen: parseTime(r.str("last_seen")), StartedAt: parseTime(r.str("started_at")),
		HeartbeatS: r.int("heartbeat_s"), Outbox: r.int("outbox"), token: r.str("token")}
	_ = json.Unmarshal([]byte(r.str("labels")), &a.Labels)
	return a
}

func (s *hubStore) agents(ctx context.Context) ([]hubAgentRow, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM agents ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := make([]hubAgentRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, agentFromRow(r))
	}
	return out, nil
}

func (s *hubStore) agentToken(ctx context.Context, name string) (string, error) {
	rows, err := s.db.Query(ctx, `SELECT token FROM agents WHERE name = ? AND revoked_at IS NULL`, name)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("unknown or revoked agent")
	}
	return rows[0].str("token"), nil
}

// --- ingest -------------------------------------------------------------------------------

// ingest stores a batch from an agent: events, and the deploys' current state.
func (s *hubStore) ingest(ctx context.Context, agent string, events []hubEvent) error {
	var stmts []sqlStmt
	now := nowText()
	for _, ev := range events {
		ev.Agent = agent // the authenticated name wins over the body
		if ev.Type == hubEventHeartbeat {
			stmts = append(stmts, s.heartbeatStmts(agent, ev, now)...)
			continue
		}
		st := ev.State
		if st == nil {
			st = &State{Deploy: ev.Deploy}
		}
		stmts = append(stmts, sqlStmt{SQL: `INSERT INTO events (id, agent, type, time, deploy, status, trigger, commit_sha, ref,
			pusher, duration, error, labels, state, log_tail, received_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`,
			Args: []any{ev.ID, agent, ev.Type, ev.Time.UTC().Format(time.RFC3339Nano), ev.Deploy, st.Status, st.Trigger, st.Commit,
				eventRef(*st), st.Pusher, st.Duration, firstNonEmpty(st.Error, ev.Reason), jsonText(st.Labels), jsonText(st),
				jsonText(ev.LogTail), now}})
		if ev.Type != hubEventRejected && ev.Deploy != "" {
			// The dashboard shows the latest state; a rejected request didn't change it.
			stmts = append(stmts, sqlStmt{SQL: `INSERT INTO deploys (agent, deploy, labels, state, status, updated_at)
				VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(agent, deploy) DO UPDATE SET labels = excluded.labels, state = excluded.state,
					status = excluded.status, updated_at = excluded.updated_at
				WHERE excluded.updated_at >= deploys.updated_at`,
				Args: []any{agent, ev.Deploy, jsonText(st.Labels), jsonText(st), st.Status, ev.Time.UTC().Format(time.RFC3339Nano)}})
		}
		stmts = append(stmts, sqlStmt{SQL: `UPDATE agents SET last_seen = ?, host = ?, version = ? WHERE name = ?`,
			Args: []any{now, ev.Host, ev.Version, agent}})
	}
	return s.db.Exec(ctx, stmts...)
}

func (s *hubStore) heartbeatStmts(agent string, ev hubEvent, now string) []sqlStmt {
	inv := ev.Inventory
	if inv == nil {
		return nil
	}
	stmts := []sqlStmt{
		{SQL: `UPDATE agents SET last_seen = ?, host = ?, version = ?, started_at = ?, outbox = ?, labels = ?, heartbeat_s = ? WHERE name = ?`,
			Args: []any{now, ev.Host, ev.Version, inv.Started.UTC().Format(time.RFC3339Nano), int64(inv.Outbox),
				jsonText(inv.Labels), inv.HeartbeatS, agent}},
		{SQL: `UPDATE deploys SET in_inventory = 0 WHERE agent = ?`, Args: []any{agent}},
	}
	for _, d := range inv.Deploys {
		st := d.State
		updated := ev.Time.UTC().Format(time.RFC3339Nano)
		if st.FinishedAt != nil {
			updated = st.FinishedAt.UTC().Format(time.RFC3339Nano)
		} else if st.StartedAt != nil {
			updated = st.StartedAt.UTC().Format(time.RFC3339Nano)
		}
		stmts = append(stmts, sqlStmt{SQL: `INSERT INTO deploys (agent, deploy, provider, repository, branch, schedule, labels, state, status, updated_at, in_inventory)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT(agent, deploy) DO UPDATE SET provider = excluded.provider, repository = excluded.repository,
				branch = excluded.branch, schedule = excluded.schedule, labels = excluded.labels, in_inventory = 1,
				state = CASE WHEN excluded.updated_at >= deploys.updated_at THEN excluded.state ELSE deploys.state END,
				status = CASE WHEN excluded.updated_at >= deploys.updated_at THEN excluded.status ELSE deploys.status END,
				updated_at = MAX(excluded.updated_at, deploys.updated_at)`,
			Args: []any{agent, d.Name, d.Provider, d.Repository, d.Branch, d.Schedule, jsonText(d.Labels), jsonText(st), st.Status, updated}})
	}
	return stmts
}

// --- queries ---------------------------------------------------------------------------------

type hubDeployRow struct {
	Agent, Deploy, Provider, Repository, Branch, Schedule string
	Labels                                                map[string]string
	State                                                 State
	UpdatedAt                                             time.Time
	InInventory                                           bool
}

func (s *hubStore) deploys(ctx context.Context) ([]hubDeployRow, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM deploys ORDER BY agent, deploy`)
	if err != nil {
		return nil, err
	}
	out := make([]hubDeployRow, 0, len(rows))
	for _, r := range rows {
		d := hubDeployRow{Agent: r.str("agent"), Deploy: r.str("deploy"), Provider: r.str("provider"), Repository: r.str("repository"),
			Branch: r.str("branch"), Schedule: r.str("schedule"), UpdatedAt: parseTime(r.str("updated_at")), InInventory: r.int("in_inventory") == 1}
		_ = json.Unmarshal([]byte(r.str("labels")), &d.Labels)
		_ = json.Unmarshal([]byte(r.str("state")), &d.State)
		if d.State.Labels == nil {
			d.State.Labels = d.Labels
		}
		out = append(out, d)
	}
	return out, nil
}

type hubEventRow struct {
	hubEvent
	Labels map[string]string
}

// events lists events newest first, optionally for one agent/deploy and type.
func (s *hubStore) events(ctx context.Context, agent, deploy, typ string, limit int) ([]hubEventRow, error) {
	var where []string
	var args []any
	if agent != "" {
		where, args = append(where, "agent = ?"), append(args, agent)
	}
	if deploy != "" {
		where, args = append(where, "deploy = ?"), append(args, deploy)
	}
	if typ != "" {
		where, args = append(where, "type = ?"), append(args, typ)
	}
	q := `SELECT * FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q += fmt.Sprintf(" ORDER BY time DESC LIMIT %d", limit)
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	out := make([]hubEventRow, 0, len(rows))
	for _, r := range rows {
		e := hubEventRow{hubEvent: hubEvent{ID: r.str("id"), Type: r.str("type"), Time: parseTime(r.str("time")),
			Agent: r.str("agent"), Deploy: r.str("deploy"), Reason: r.str("error")}}
		var st State
		if json.Unmarshal([]byte(r.str("state")), &st) == nil {
			e.State = &st
		}
		_ = json.Unmarshal([]byte(r.str("log_tail")), &e.LogTail)
		_ = json.Unmarshal([]byte(r.str("labels")), &e.Labels)
		out = append(out, e)
	}
	return out, nil
}

// prune deletes events older than the retention.
func (s *hubStore) prune(ctx context.Context, days int) error {
	if days <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	return s.db.Exec(ctx, sqlStmt{SQL: `DELETE FROM events WHERE time < ?`, Args: []any{cutoff}})
}

// labelValues lists the values seen for a label key, for the dashboard filters.
func labelValues(deploys []hubDeployRow, key string) []string {
	seen := map[string]bool{}
	for _, d := range deploys {
		if v := d.Labels[key]; v != "" {
			seen[v] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
