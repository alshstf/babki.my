package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"babki.my/babki/internal/platform/jobs"
	"babki.my/babki/internal/platform/testdb"
)

func TestRootHasCommands(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute --help: %v", err)
	}
	out := buf.String()
	for _, cmd := range []string{"all", "api", "worker", "migrate", "version"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("help output missing command %q; got:\n%s", cmd, out)
		}
	}
}

// The cbr client is bounded; cbr.New(nil, "") would use http.DefaultClient
// with no timeout, and one stalled connection could hold a worker for the job's
// 15 minutes.
func TestNewCbrHTTPClientHasABoundedTimeout(t *testing.T) {
	c := newCbrHTTPClient()
	if c.Timeout != cbrHTTPTimeout {
		t.Fatalf("newCbrHTTPClient().Timeout = %s, want %s", c.Timeout, cbrHTTPTimeout)
	}
	if c.Timeout <= 0 {
		t.Fatalf("newCbrHTTPClient().Timeout = %s, want a positive bound (0 means unbounded)", c.Timeout)
	}
}

// TestNewMoexHTTPClientHasABoundedTimeout: the same guard for MOEX ISS, which
// used to run on http.DefaultClient with no bound at all (#191).
func TestNewMoexHTTPClientHasABoundedTimeout(t *testing.T) {
	c := newMoexHTTPClient()
	if c.Timeout != moexHTTPTimeout || c.Timeout <= 0 {
		t.Fatalf("newMoexHTTPClient().Timeout = %s, want the positive bound %s", c.Timeout, moexHTTPTimeout)
	}
}

// jobs.SoftStopTimeout must stay strictly below stopJobClientTimeout; the two
// live in different packages, and equal values race. Otherwise every full soft
// stop would escalate and cancel the jobs it meant to spare.
func TestTheJobQueueIsGivenLessTimeToStopThanTheProcessWaitsForIt(t *testing.T) {
	if jobs.SoftStopTimeout <= 0 {
		t.Fatalf("jobs.SoftStopTimeout = %s; a non-positive value is how River spells "+
			"\"no graceful window at all\"", jobs.SoftStopTimeout)
	}
	if jobs.SoftStopTimeout >= stopJobClientTimeout {
		t.Fatalf("jobs.SoftStopTimeout = %s, stopJobClientTimeout = %s: the inner bound must be "+
			"strictly the shorter, or the graceful stop is abandoned while jobs are still inside "+
			"their window", jobs.SoftStopTimeout, stopJobClientTimeout)
	}
}

// testSetupCode is the first-run code the roles in these tests ask for.
const testSetupCode = "TESTCODE"

// roleEnv points the configuration at a private test database (its URL read off
// the pool testdb returned), supplies what the key-requiring roles demand, and
// returns the address the HTTP roles will listen on.
func roleEnv(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	addr := freePort(t)
	t.Setenv("BABKI_DATABASE_URL", pool.Config().ConnString())
	t.Setenv("BABKI_ENCRYPTION_KEY", validHexKey)
	t.Setenv("BABKI_HTTP_ADDR", addr)
	t.Setenv("BABKI_LOG_LEVEL", "error")
	t.Setenv("BABKI_SETUP_CODE", testSetupCode)
	return addr
}

// freePort binds a port, releases it and returns the address (httpserver's
// trick; test helpers cannot be shared). If it is taken meanwhile, the role
// fails to listen and the assertions see the error.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// A smoke test of the "api" role from argv: it answers on its port and stops
// cleanly when its context ends. It covers cmd/babki's wiring, not module
// behaviour. "worker" and "all" are not run here: their periodic jobs start at
// once against the production cbr.ru and iss.moex.com; the queue's startup is
// tested against stubs in internal/background.
func TestAPIRoleServesHealthzAndStopsCleanly(t *testing.T) {
	pool := testdb.New(t)
	addr := roleEnv(t, pool)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := newRootCmd()
	root.SetArgs([]string{"api"})
	root.SetOut(io.Discard)
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	var body []byte
	for {
		resp, err := client.Get("http://" + addr + "/api/healthz")
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
			err = errors.New("status " + resp.Status + ", body " + string(body))
		}
		select {
		case runErr := <-done:
			t.Fatalf("the api role exited before it answered: %v", runErr)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("the api role never answered on %s: %v", addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("healthz answered %s, want status ok: the role is up but its database is not", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the api role returned %v on shutdown, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the api role did not return after its context was cancelled")
	}
}

// migrate applies the schema to an empty database and exits, with no
// encryption key set, which is its promise.
func TestMigrateRoleAppliesTheSchemaAndExits(t *testing.T) {
	pool := testdb.NewEmpty(t)
	ctx := context.Background()

	t.Setenv("BABKI_DATABASE_URL", pool.Config().ConnString())
	t.Setenv("BABKI_ENCRYPTION_KEY", "")
	t.Setenv("BABKI_LOG_LEVEL", "error")

	var before bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.accounts') IS NOT NULL`).Scan(&before); err != nil {
		t.Fatalf("look for the accounts table before migrating: %v", err)
	}
	if before {
		t.Fatal("the database already has a schema; this test needs an empty one to prove anything")
	}

	root := newRootCmd()
	root.SetArgs([]string{"migrate"})
	root.SetOut(io.Discard)
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("migrate role: %v", err)
	}

	var after bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.accounts') IS NOT NULL`).Scan(&after); err != nil {
		t.Fatalf("look for the accounts table after migrating: %v", err)
	}
	if !after {
		t.Error("the migrate role returned success and left no schema behind")
	}
}
