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

const hubSchemaVersion = 6

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

// hubMigrations take the schema from version i to i+1 (index 0: 1 → 2).
var hubMigrations = [][]string{
	{
		`ALTER TABLE agents ADD COLUMN client TEXT`,
		`ALTER TABLE agents ADD COLUMN env TEXT`,
		`ALTER TABLE agents ADD COLUMN require_cert INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE agents ADD COLUMN cert_fp TEXT`,
		`ALTER TABLE agents ADD COLUMN cert_not_after TEXT`,
		`ALTER TABLE agents ADD COLUMN accepts_commands INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS heartbeats (agent TEXT NOT NULL, minute TEXT NOT NULL, PRIMARY KEY (agent, minute))`,
		`CREATE TABLE IF NOT EXISTS commands (
			id          TEXT PRIMARY KEY,
			agent       TEXT NOT NULL,
			deploy      TEXT NOT NULL,
			action      TEXT NOT NULL,
			commit_sha  TEXT,
			params      TEXT,
			created_by  TEXT,
			created_at  TEXT NOT NULL,
			status      TEXT NOT NULL,
			sent_at     TEXT,
			result      TEXT,
			updated_at  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS commands_agent ON commands (agent, status, created_at)`,
	},
	{
		`CREATE TABLE IF NOT EXISTS notify_channels (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			kind       TEXT NOT NULL,
			config     TEXT NOT NULL,
			created_at TEXT NOT NULL,
			last_at    TEXT,
			last_error TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS notify_rules (
			id         TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			lang       TEXT,
			enabled    INTEGER NOT NULL DEFAULT 1,
			events     TEXT NOT NULL,
			clients    TEXT,
			envs       TEXT,
			agents     TEXT,
			deploys    TEXT,
			channels   TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS notify_state (key TEXT PRIMARY KEY, value TEXT, updated_at TEXT NOT NULL)`,
	},
	{
		`ALTER TABLE notify_rules ADD COLUMN kinds TEXT`,
	},
	{
		`ALTER TABLE agents ADD COLUMN cert_fp_prev TEXT`,
		`ALTER TABLE agents ADD COLUMN cert_renewed_at TEXT`,
		`ALTER TABLE agents ADD COLUMN cert_auto_renew INTEGER NOT NULL DEFAULT 0`,
	},
	{
		`CREATE TABLE IF NOT EXISTS oauth_clients (
			id            TEXT PRIMARY KEY,
			name          TEXT,
			redirect_uris TEXT NOT NULL,
			created_at    TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_grants (
			id         TEXT PRIMARY KEY,
			client_id  TEXT NOT NULL,
			name       TEXT NOT NULL,
			created_by TEXT,
			created_at TEXT NOT NULL,
			last_used  TEXT,
			revoked_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_tokens (
			hash       TEXT PRIMARY KEY,
			grant_id   TEXT NOT NULL,
			kind       TEXT NOT NULL,
			expires_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS oauth_tokens_grant ON oauth_tokens (grant_id)`,
	},
}

func (s *hubStore) migrate(ctx context.Context) error {
	for _, q := range hubSchema {
		if err := s.db.Exec(ctx, sqlStmt{SQL: q}); err != nil {
			return err
		}
	}
	rows, err := s.db.Query(ctx, `SELECT value FROM hub_meta WHERE key = 'schema'`)
	if err != nil {
		return err
	}
	have := 1
	if len(rows) == 1 {
		fmt.Sscan(rows[0].str("value"), &have)
	}
	for v := have; v < hubSchemaVersion; v++ {
		stmts := make([]sqlStmt, 0, len(hubMigrations[v-1])+1)
		for _, q := range hubMigrations[v-1] {
			stmts = append(stmts, sqlStmt{SQL: q})
		}
		stmts = append(stmts, sqlStmt{SQL: `INSERT INTO hub_meta (key, value) VALUES ('schema', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, Args: []any{fmt.Sprint(v + 1)}})
		if err := s.db.Exec(ctx, stmts...); err != nil {
			return fmt.Errorf("migrating the hub database to version %d: %w", v+1, err)
		}
	}
	return nil
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
	// Client and Env are what the agent was created for; once it reports,
	// its labels say the same (or override them).
	Client, Env     string
	RequireCert     bool
	CertFP          string
	CertNotAfter    time.Time
	AcceptsCommands bool
	// CertFPPrev is the certificate before the last renewal: same key, so
	// still accepted until the agent's connections pick up the new one.
	CertFPPrev    string
	CertRenewedAt time.Time
	CertAutoRenew bool // the agent renews its certificate itself
	token         string
}

// ClientName is the agent's client: its label, else what it was created for.
func (a hubAgentRow) ClientName() string { return firstNonEmpty(a.Labels["client"], a.Client) }

// EnvName is the agent's environment, likewise.
func (a hubAgentRow) EnvName() string { return firstNonEmpty(a.Labels["environment"], a.Env) }

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
	return s.createAgent(ctx, newAgentOpts{Name: name})
}

type newAgentOpts struct {
	Name, Client, Env string
	RequireCert       bool
	CertFP            string
	CertNotAfter      time.Time
}

// createAgent creates an agent or renews an existing one: a new token, and
// with a certificate its new fingerprint (the old one stops working).
func (s *hubStore) createAgent(ctx context.Context, o newAgentOpts) (string, error) {
	if !agentNameRe.MatchString(o.Name) {
		return "", fmt.Errorf("agent name %q: letters, digits, _ . - (max 64)", o.Name)
	}
	if len(o.Client) > 200 || len(o.Env) > 200 || strings.ContainsAny(o.Client+o.Env, "\n\r\t") {
		return "", errors.New("client and environment: up to 200 characters, one line")
	}
	token := newAgentToken()
	var notAfter any
	if !o.CertNotAfter.IsZero() {
		notAfter = o.CertNotAfter.UTC().Format(time.RFC3339)
	}
	err := s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO agents (name, token, created_at, client, env, require_cert, cert_fp, cert_not_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET token = excluded.token, revoked_at = NULL,
			client = COALESCE(NULLIF(excluded.client, ''), agents.client), env = COALESCE(NULLIF(excluded.env, ''), agents.env),
			require_cert = excluded.require_cert, cert_fp = excluded.cert_fp, cert_not_after = excluded.cert_not_after,
			cert_fp_prev = NULL, cert_renewed_at = NULL`,
		Args: []any{o.Name, token, nowText(), o.Client, o.Env, o.RequireCert, nullIfEmpty(o.CertFP), notAfter}})
	return token, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *hubStore) agent(ctx context.Context, name string) (hubAgentRow, bool, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM agents WHERE name = ?`, name)
	if err != nil || len(rows) == 0 {
		return hubAgentRow{}, false, err
	}
	return agentFromRow(rows[0]), true, nil
}

// renewAgentCert records an agent's renewed certificate. The one it renewed
// with (usedFP) stays accepted: it has the same key.
func (s *hubStore) renewAgentCert(ctx context.Context, name, usedFP, newFP string, notAfter time.Time) error {
	return s.db.Exec(ctx, sqlStmt{SQL: `UPDATE agents SET cert_fp_prev = ?, cert_fp = ?, cert_not_after = ?, cert_renewed_at = ? WHERE name = ?`,
		Args: []any{usedFP, newFP, notAfter.UTC().Format(time.RFC3339), nowText(), name}})
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
		HeartbeatS: r.int("heartbeat_s"), Outbox: r.int("outbox"), token: r.str("token"),
		Client: r.str("client"), Env: r.str("env"), RequireCert: r.int("require_cert") == 1, CertFP: r.str("cert_fp"),
		CertNotAfter: parseTime(r.str("cert_not_after")), AcceptsCommands: r.int("accepts_commands") == 1,
		CertFPPrev: r.str("cert_fp_prev"), CertRenewedAt: parseTime(r.str("cert_renewed_at")), CertAutoRenew: r.int("cert_auto_renew") == 1}
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
		if ev.Type == hubEventCommandRefused {
			stmts = append(stmts, sqlStmt{SQL: `UPDATE commands SET status = 'refused', result = ?, updated_at = ? WHERE id = ? AND agent = ?`,
				Args: []any{clip(ev.Reason, 300), now, ev.ID, agent}})
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
		// A run started by a hub command reports back under its delivery.
		if id, ok := strings.CutPrefix(st.Delivery, hubCommandDelivery); ok && ev.Type != hubEventRejected {
			status := "running"
			if ev.Type == hubEventDeployDone {
				status = "done"
			}
			stmts = append(stmts, sqlStmt{SQL: `UPDATE commands SET status = ?, result = ?, updated_at = ? WHERE id = ? AND agent = ?`,
				Args: []any{status, st.Status, now, id, agent}})
		}
	}
	return s.db.Exec(ctx, stmts...)
}

func (s *hubStore) heartbeatStmts(agent string, ev hubEvent, now string) []sqlStmt {
	inv := ev.Inventory
	if inv == nil {
		return nil
	}
	stmts := []sqlStmt{
		{SQL: `UPDATE agents SET last_seen = ?, host = ?, version = ?, started_at = ?, outbox = ?, labels = ?, heartbeat_s = ?, accepts_commands = ?, cert_auto_renew = ? WHERE name = ?`,
			Args: []any{now, ev.Host, ev.Version, inv.Started.UTC().Format(time.RFC3339Nano), int64(inv.Outbox),
				jsonText(inv.Labels), inv.HeartbeatS, inv.AcceptsCommands, inv.CertRenew, agent}},
		{SQL: `INSERT INTO heartbeats (agent, minute) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			Args: []any{agent, time.Now().UTC().Truncate(time.Minute).Format(time.RFC3339)}},
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
	beats := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	return s.db.Exec(ctx, sqlStmt{SQL: `DELETE FROM events WHERE time < ?`, Args: []any{cutoff}},
		sqlStmt{SQL: `DELETE FROM commands WHERE created_at < ?`, Args: []any{cutoff}},
		sqlStmt{SQL: `DELETE FROM heartbeats WHERE minute < ?`, Args: []any{beats}},
		sqlStmt{SQL: `DELETE FROM oauth_tokens WHERE expires_at < ?`, Args: []any{nowText()}})
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

// --- dashboard data ---------------------------------------------------------------------------

// beats says, for each of the last n minutes (oldest first), whether the agent sent a heartbeat.
func (s *hubStore) beats(ctx context.Context, n int) (map[string][]bool, error) {
	since := time.Now().UTC().Truncate(time.Minute).Add(-time.Duration(n-1) * time.Minute)
	rows, err := s.db.Query(ctx, `SELECT agent, minute FROM heartbeats WHERE minute >= ?`, since.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	out := map[string][]bool{}
	for _, r := range rows {
		t, err := time.Parse(time.RFC3339, r.str("minute"))
		if err != nil {
			continue
		}
		i := int(t.Sub(since) / time.Minute)
		if i < 0 || i >= n {
			continue
		}
		a := r.str("agent")
		if out[a] == nil {
			out[a] = make([]bool, n)
		}
		out[a][i] = true
	}
	return out, nil
}

// dayCount is one day of finished runs by result.
type dayCount struct {
	Day                    time.Time
	Success, Failed, Other int
}

// daily counts finished runs per day for the last n days, oldest first.
func (s *hubStore) daily(ctx context.Context, n int) ([]dayCount, error) {
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(n - 1))
	rows, err := s.db.Query(ctx, `SELECT substr(time, 1, 10) AS day, status, count(*) AS n FROM events
		WHERE type = ? AND time >= ? GROUP BY day, status`, hubEventDeployDone, start.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	out := make([]dayCount, n)
	for i := range out {
		out[i].Day = start.AddDate(0, 0, i)
	}
	for _, r := range rows {
		day, err := time.Parse("2006-01-02", r.str("day"))
		if err != nil {
			continue
		}
		i := int(day.Sub(start).Hours() / 24)
		if i < 0 || i >= n {
			continue
		}
		switch r.str("status") {
		case StatusSuccess:
			out[i].Success += int(r.int("n"))
		case StatusFailed:
			out[i].Failed += int(r.int("n"))
		default:
			out[i].Other += int(r.int("n"))
		}
	}
	return out, nil
}

// runs lists a deploy's finished runs, newest first.
func (s *hubStore) runs(ctx context.Context, agent, deploy string, limit int) ([]State, error) {
	rows, err := s.db.Query(ctx, `SELECT state FROM events WHERE agent = ? AND deploy = ? AND type = ? ORDER BY time DESC LIMIT `+fmt.Sprint(limit),
		agent, deploy, hubEventDeployDone)
	if err != nil {
		return nil, err
	}
	out := make([]State, 0, len(rows))
	for _, r := range rows {
		var st State
		if json.Unmarshal([]byte(r.str("state")), &st) == nil {
			out = append(out, st)
		}
	}
	return out, nil
}

// recentStatuses is each deploy's last runs (newest first) as status letters.
func (s *hubStore) recentStatuses(ctx context.Context, perDeploy int) (map[[2]string][]string, error) {
	since := time.Now().UTC().AddDate(0, 0, -30).Format(time.RFC3339)
	rows, err := s.db.Query(ctx, `SELECT agent, deploy, status FROM events WHERE type = ? AND time >= ? ORDER BY time DESC LIMIT 5000`,
		hubEventDeployDone, since)
	if err != nil {
		return nil, err
	}
	out := map[[2]string][]string{}
	for _, r := range rows {
		k := [2]string{r.str("agent"), r.str("deploy")}
		if len(out[k]) < perDeploy {
			out[k] = append(out[k], r.str("status"))
		}
	}
	return out, nil
}

// --- commands -----------------------------------------------------------------------------------

const (
	hubCommandDelivery = "hub-"           // delivery of a run started by a command: hub-<id>
	hubCommandTTL      = 10 * time.Minute // a command not picked up by then expires
)

// hubCommand is an order for an agent: run a deploy again, or roll it back.
type hubCommand struct {
	ID        string            `json:"id"`
	Agent     string            `json:"-"`
	Deploy    string            `json:"deploy"`
	Action    string            `json:"action"` // run | rollback
	Commit    string            `json:"commit,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	By        string            `json:"by"`
	CreatedAt time.Time         `json:"created_at"`
	Status    string            `json:"-"` // pending, sent, running, done, refused, expired
	Result    string            `json:"-"`
}

func (s *hubStore) createCommand(ctx context.Context, c hubCommand) error {
	return s.db.Exec(ctx, sqlStmt{SQL: `INSERT INTO commands (id, agent, deploy, action, commit_sha, params, created_by, created_at, status, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		Args: []any{c.ID, c.Agent, c.Deploy, c.Action, nullIfEmpty(c.Commit), jsonText(c.Params), c.By, c.CreatedAt.UTC().Format(time.RFC3339Nano), nowText()}})
}

func commandFromRow(r sqlRow) hubCommand {
	c := hubCommand{ID: r.str("id"), Agent: r.str("agent"), Deploy: r.str("deploy"), Action: r.str("action"), Commit: r.str("commit_sha"),
		By: r.str("created_by"), CreatedAt: parseTime(r.str("created_at")), Status: r.str("status"), Result: r.str("result")}
	_ = json.Unmarshal([]byte(r.str("params")), &c.Params)
	return c
}

// takeCommands hands an agent its pending commands and marks them sent;
// the ones older than hubCommandTTL expire instead.
func (s *hubStore) takeCommands(ctx context.Context, agent string) ([]hubCommand, error) {
	now := time.Now().UTC()
	expired := now.Add(-hubCommandTTL).Format(time.RFC3339Nano)
	if err := s.db.Exec(ctx, sqlStmt{SQL: `UPDATE commands SET status = 'expired', updated_at = ? WHERE agent = ? AND status = 'pending' AND created_at < ?`,
		Args: []any{nowText(), agent, expired}}); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT * FROM commands WHERE agent = ? AND status = 'pending' ORDER BY created_at`, agent)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]hubCommand, 0, len(rows))
	stmts := make([]sqlStmt, 0, len(rows))
	for _, r := range rows {
		c := commandFromRow(r)
		out = append(out, c)
		stmts = append(stmts, sqlStmt{SQL: `UPDATE commands SET status = 'sent', sent_at = ?, updated_at = ? WHERE id = ? AND status = 'pending'`,
			Args: []any{nowText(), nowText(), c.ID}})
	}
	return out, s.db.Exec(ctx, stmts...)
}

// commandsFor lists the latest commands for one deploy (or all, with deploy "").
func (s *hubStore) commandsFor(ctx context.Context, agent, deploy string, limit int) ([]hubCommand, error) {
	q, args := `SELECT * FROM commands WHERE agent = ?`, []any{agent}
	if deploy != "" {
		q, args = q+` AND deploy = ?`, append(args, deploy)
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY created_at DESC LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		return nil, err
	}
	out := make([]hubCommand, 0, len(rows))
	for _, r := range rows {
		c := commandFromRow(r)
		if c.Status == "pending" && time.Since(c.CreatedAt) > hubCommandTTL {
			c.Status = "expired"
		}
		out = append(out, c)
	}
	return out, nil
}
