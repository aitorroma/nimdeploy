package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A small libSQL client over its HTTP API (Hrana over HTTP, /v2/pipeline),
// for a self-hosted libSQL server (sqld) or Turso. Pure Go, so nimdeploy
// stays a single static binary; the data lives in sqld's SQLite file.

type libsqlClient struct {
	endpoint string // .../v2/pipeline
	token    string
	http     *http.Client
}

func newLibsqlClient(rawURL, token string) (*libsqlClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("database_url %q: want http(s)://host:port or libsql://host", rawURL)
	}
	switch u.Scheme {
	case "libsql", "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	case "http", "https":
	default:
		return nil, fmt.Errorf("database_url %q: scheme must be http, https or libsql", rawURL)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v2/pipeline"
	return &libsqlClient{endpoint: u.String(), token: token, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

type sqlStmt struct {
	SQL  string
	Args []any
}

type hranaValue struct {
	Type   string `json:"type"`
	Value  any    `json:"value,omitempty"`
	Base64 string `json:"base64,omitempty"` // blobs
}

func toHrana(v any) (hranaValue, error) {
	switch x := v.(type) {
	case nil:
		return hranaValue{Type: "null"}, nil
	case string:
		return hranaValue{Type: "text", Value: x}, nil
	case []byte:
		return hranaValue{Type: "blob", Base64: base64.StdEncoding.EncodeToString(x)}, nil
	case bool:
		if x {
			return hranaValue{Type: "integer", Value: "1"}, nil
		}
		return hranaValue{Type: "integer", Value: "0"}, nil
	case int:
		return hranaValue{Type: "integer", Value: strconv.Itoa(x)}, nil
	case int64:
		return hranaValue{Type: "integer", Value: strconv.FormatInt(x, 10)}, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return hranaValue{}, errors.New("NaN/Inf cannot be stored")
		}
		return hranaValue{Type: "float", Value: x}, nil
	case time.Time:
		return hranaValue{Type: "text", Value: x.UTC().Format(time.RFC3339Nano)}, nil
	}
	return hranaValue{}, fmt.Errorf("unsupported SQL argument %T", v)
}

func fromHrana(v hranaValue) any {
	switch v.Type {
	case "integer":
		s, _ := v.Value.(string)
		n, _ := strconv.ParseInt(s, 10, 64)
		return n
	case "float":
		f, _ := v.Value.(float64)
		return f
	case "text":
		s, _ := v.Value.(string)
		return s
	case "blob":
		b, err := base64.StdEncoding.DecodeString(v.Base64)
		if err != nil {
			b, _ = base64.RawStdEncoding.DecodeString(v.Base64)
		}
		return b
	}
	return nil
}

type hranaStmt struct {
	SQL      string       `json:"sql"`
	Args     []hranaValue `json:"args,omitempty"`
	WantRows bool         `json:"want_rows"`
}

type hranaResult struct {
	Cols []struct {
		Name string `json:"name"`
	} `json:"cols"`
	Rows             [][]hranaValue `json:"rows"`
	AffectedRowCount int64          `json:"affected_row_count"`
}

type hranaError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// sqlRow maps column names to Go values (int64, float64, string, []byte, nil).
type sqlRow map[string]any

func (r sqlRow) str(col string) string {
	switch v := r[col].(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []byte:
		return string(v)
	}
	return ""
}

func (r sqlRow) int(col string) int64 {
	switch v := r[col].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

func (c *libsqlClient) stmt(s sqlStmt, wantRows bool) (hranaStmt, error) {
	hs := hranaStmt{SQL: s.SQL, WantRows: wantRows}
	for _, a := range s.Args {
		v, err := toHrana(a)
		if err != nil {
			return hs, err
		}
		hs.Args = append(hs.Args, v)
	}
	return hs, nil
}

func (c *libsqlClient) pipeline(ctx context.Context, requests []any) ([]json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"requests": append(requests, map[string]string{"type": "close"})})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, fmt.Errorf("libsql: %w", uerr.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("libsql: HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(b)), 300))
	}
	var out struct {
		Results []struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Error    *hranaError     `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("libsql: bad response: %w", err)
	}
	var res []json.RawMessage
	for i, r := range out.Results {
		if i >= len(requests) {
			break // the "close" request
		}
		if r.Type != "ok" {
			msg := "unknown error"
			if r.Error != nil {
				msg = r.Error.Message
			}
			return nil, fmt.Errorf("libsql: %s", msg)
		}
		res = append(res, r.Response)
	}
	return res, nil
}

// Query runs one statement and returns its rows.
func (c *libsqlClient) Query(ctx context.Context, sql string, args ...any) ([]sqlRow, error) {
	hs, err := c.stmt(sqlStmt{SQL: sql, Args: args}, true)
	if err != nil {
		return nil, err
	}
	res, err := c.pipeline(ctx, []any{map[string]any{"type": "execute", "stmt": hs}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result hranaResult `json:"result"`
	}
	if err := json.Unmarshal(res[0], &r); err != nil {
		return nil, err
	}
	return rowsOf(r.Result), nil
}

func rowsOf(r hranaResult) []sqlRow {
	rows := make([]sqlRow, 0, len(r.Rows))
	for _, row := range r.Rows {
		m := sqlRow{}
		for i, v := range row {
			if i < len(r.Cols) {
				m[r.Cols[i].Name] = fromHrana(v)
			}
		}
		rows = append(rows, m)
	}
	return rows
}

// Exec runs statements in one transaction: all of them or none.
func (c *libsqlClient) Exec(ctx context.Context, stmts ...sqlStmt) error {
	if len(stmts) == 0 {
		return nil
	}
	type cond struct {
		Type  string `json:"type"`
		Step  *int   `json:"step,omitempty"`
		Cond  *cond  `json:"cond,omitempty"`
		Conds []cond `json:"conds,omitempty"`
	}
	type step struct {
		Condition *cond     `json:"condition,omitempty"`
		Stmt      hranaStmt `json:"stmt"`
	}
	steps := []step{{Stmt: hranaStmt{SQL: "BEGIN"}}}
	for _, s := range stmts {
		hs, err := c.stmt(s, false)
		if err != nil {
			return err
		}
		prev := len(steps) - 1
		steps = append(steps, step{Condition: &cond{Type: "ok", Step: &prev}, Stmt: hs})
	}
	last := len(steps) - 1
	steps = append(steps, step{Condition: &cond{Type: "ok", Step: &last}, Stmt: hranaStmt{SQL: "COMMIT"}})
	commit := len(steps) - 1
	steps = append(steps, step{Condition: &cond{Type: "not", Cond: &cond{Type: "ok", Step: &commit}}, Stmt: hranaStmt{SQL: "ROLLBACK"}})

	res, err := c.pipeline(ctx, []any{map[string]any{"type": "batch", "batch": map[string]any{"steps": steps}}})
	if err != nil {
		return err
	}
	var r struct {
		Result struct {
			StepErrors []*hranaError `json:"step_errors"`
		} `json:"result"`
	}
	if err := json.Unmarshal(res[0], &r); err != nil {
		return err
	}
	for i, e := range r.Result.StepErrors {
		if e != nil && i < len(steps)-1 { // a failing ROLLBACK after a commit is not an error
			return fmt.Errorf("libsql: %s", e.Message)
		}
	}
	return nil
}
