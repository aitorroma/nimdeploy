package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// testHubStore needs a libSQL server: NIMDEPLOY_TEST_LIBSQL=http://127.0.0.1:8080
// (docker run -p 8080:8080 ghcr.io/tursodatabase/libsql-server). It starts
// from empty tables, so don't point it at a real hub.
func testHubStore(t *testing.T) *hubStore {
	t.Helper()
	url := os.Getenv("NIMDEPLOY_TEST_LIBSQL")
	if url == "" {
		t.Skip("NIMDEPLOY_TEST_LIBSQL not set")
	}
	db, err := newLibsqlClient(url, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tbl := range []string{"events", "deploys", "agents", "hub_meta"} {
		if err := db.Exec(ctx, sqlStmt{SQL: "DROP TABLE IF EXISTS " + tbl}); err != nil {
			t.Fatal(err)
		}
	}
	s := &hubStore{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLibsqlClient(t *testing.T) {
	s := testHubStore(t)
	ctx := context.Background()
	db := s.db
	if err := db.Exec(ctx, sqlStmt{SQL: "DROP TABLE IF EXISTS t"}, sqlStmt{SQL: "CREATE TABLE t (a TEXT, b INTEGER, c REAL, d BLOB, e TEXT)"}); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	if err := db.Exec(ctx, sqlStmt{SQL: "INSERT INTO t VALUES (?, ?, ?, ?, ?)", Args: []any{"it's", int64(1) << 40, 1.5, []byte{0, 1, 2}, when}}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, "SELECT * FROM t WHERE b > ?", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	r := rows[0]
	if r.str("a") != "it's" || r.int("b") != 1<<40 || r["c"] != 1.5 || string(r["d"].([]byte)) != "\x00\x01\x02" || r.str("e") != "2026-10-07T09:00:00Z" {
		t.Errorf("row %#v", r)
	}

	// A failing statement rolls the whole batch back.
	err = db.Exec(ctx, sqlStmt{SQL: "INSERT INTO t (a) VALUES ('x')"}, sqlStmt{SQL: "INSERT INTO nope VALUES (1)"})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want error, got %v", err)
	}
	if rows, _ := db.Query(ctx, "SELECT count(*) AS n FROM t"); rows[0].int("n") != 1 {
		t.Errorf("not rolled back: %v", rows)
	}
	// And the connection is usable afterwards.
	if err := db.Exec(ctx, sqlStmt{SQL: "DROP TABLE t"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "SELEC 1"); err == nil {
		t.Error("syntax error not reported")
	}
}

func TestHubStore(t *testing.T) {
	s := testHubStore(t)
	ctx := context.Background()
	tok, err := s.addAgent(ctx, "stage-1")
	if err != nil || len(tok) != 64 {
		t.Fatal(tok, err)
	}
	if got, _ := s.agentToken(ctx, "stage-1"); got != tok {
		t.Fatal("token")
	}
	if _, err := s.addAgent(ctx, "bad name"); err == nil {
		t.Error("bad agent name accepted")
	}

	t0 := time.Now().UTC().Add(-time.Minute)
	t1 := t0.Add(30 * time.Second)
	labels := map[string]string{"client": "Acme", "environment": "stage"}
	started := &State{Deploy: "web", Status: StatusRunning, Commit: "abc", Labels: labels, StartedAt: &t0}
	finished := &State{Deploy: "web", Status: StatusSuccess, Commit: "abc", Labels: labels, StartedAt: &t0, FinishedAt: &t1}
	err = s.ingest(ctx, "stage-1", []hubEvent{
		{ID: "e1", Type: hubEventDeployStart, Time: t0, Deploy: "web", State: started, Host: "h1", Version: "v1"},
		{ID: "e2", Type: hubEventDeployDone, Time: t1, Deploy: "web", State: finished, Host: "h1", Version: "v1"},
		{ID: "e2", Type: hubEventDeployDone, Time: t1, Deploy: "web", State: finished}, // duplicate
		{ID: "e3", Type: hubEventRejected, Time: t1, Deploy: "web", Reason: "bad signature"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// An older event arriving late doesn't overwrite the newer state.
	if err := s.ingest(ctx, "stage-1", []hubEvent{{ID: "e0", Type: hubEventDeployStart, Time: t0.Add(-time.Hour), Deploy: "web", State: started}}); err != nil {
		t.Fatal(err)
	}
	deps, _ := s.deploys(ctx)
	if len(deps) != 1 || deps[0].State.Status != StatusSuccess || deps[0].Labels["client"] != "Acme" {
		t.Fatalf("deploys %+v", deps)
	}
	evs, _ := s.events(ctx, "stage-1", "web", "", 0)
	if len(evs) != 4 || evs[0].Type != hubEventRejected && evs[0].Type != hubEventDeployDone {
		t.Fatalf("events %+v", evs)
	}
	if evs, _ := s.events(ctx, "", "", hubEventRejected, 0); len(evs) != 1 || evs[0].Reason != "bad signature" {
		t.Errorf("rejected %+v", evs)
	}

	// Heartbeat: agent info, inventory adds a never-run deploy, drops "web".
	inv := &hubInventory{Started: t0, Labels: map[string]string{"client": "Acme"}, Outbox: 3, HeartbeatS: 60,
		Deploys: []hubDeploy{{Name: "api", Provider: "github", Repository: "acme/api", Branch: "main", State: State{Deploy: "api", Status: StatusNever}}}}
	if err := s.ingest(ctx, "stage-1", []hubEvent{{ID: "hb", Type: hubEventHeartbeat, Time: time.Now().UTC(), Host: "h1", Version: "v2", Inventory: inv}}); err != nil {
		t.Fatal(err)
	}
	agents, _ := s.agents(ctx)
	if len(agents) != 1 || agents[0].Version != "v2" || agents[0].Outbox != 3 || !agents[0].online(time.Now()) || agents[0].Labels["client"] != "Acme" {
		t.Fatalf("agents %+v", agents)
	}
	if agents[0].online(time.Now().Add(10 * time.Minute)) {
		t.Error("should be offline")
	}
	deps, _ = s.deploys(ctx)
	if len(deps) != 2 || deps[0].Deploy != "api" || !deps[0].InInventory || deps[0].Repository != "acme/api" || deps[1].InInventory {
		t.Fatalf("after heartbeat %+v", deps)
	}
	// Heartbeats aren't stored as events.
	if evs, _ := s.events(ctx, "", "", hubEventHeartbeat, 0); len(evs) != 0 {
		t.Error("heartbeat stored")
	}

	if err := s.revokeAgent(ctx, "stage-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.agentToken(ctx, "stage-1"); err == nil {
		t.Error("revoked agent still has a token")
	}
	if err := s.prune(ctx, 1); err != nil {
		t.Fatal(err)
	}
}
