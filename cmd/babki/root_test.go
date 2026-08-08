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

// TestNewCbrHTTPClientHasABoundedTimeout guards against startJobClient going
// back to an unbounded client (cbr.New(nil, "") falls back to
// http.DefaultClient, whose Timeout is 0): the backfill job fires one
// request per currency in use under a 15-minute job Timeout, so one stalled
// TCP connection on an unbounded client could pin a worker slot for that
// whole budget.
func TestNewCbrHTTPClientHasABoundedTimeout(t *testing.T) {
	c := newCbrHTTPClient()
	if c.Timeout != cbrHTTPTimeout {
		t.Fatalf("newCbrHTTPClient().Timeout = %s, want %s", c.Timeout, cbrHTTPTimeout)
	}
	if c.Timeout <= 0 {
		t.Fatalf("newCbrHTTPClient().Timeout = %s, want a positive bound (0 means unbounded)", c.Timeout)
	}
}

// TestTheJobQueueIsGivenLessTimeToStopThanTheProcessWaitsForIt keeps two
// timeouts in the order their comments claim they are in. They live in
// different packages and neither can see the other's value, so nothing but
// this stops someone raising jobs.SoftStopTimeout past the bound that covers
// it — after which every shutdown that used the whole soft window would report
// a graceful stop that "did not complete in time" and escalate to
// StopAndCancel, cancelling exactly the jobs the soft window exists to spare.
//
// STRICTLY LESS, not less-or-equal: equal values race, and which of the two
// fires first would decide whether a shutdown looked clean or looked broken.
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

// roleEnv points the process configuration at a private test database and
// gives it everything the roles that decrypt secrets demand, then hands back
// the address the HTTP roles will listen on.
//
// The database URL is read off the pool testdb hands out rather than rebuilt,
// so the role under test connects to the very database this test owns.
func roleEnv(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	addr := freePort(t)
	t.Setenv("BABKI_DATABASE_URL", pool.Config().ConnString())
	t.Setenv("BABKI_ENCRYPTION_KEY", validHexKey)
	t.Setenv("BABKI_HTTP_ADDR", addr)
	t.Setenv("BABKI_LOG_LEVEL", "error")
	return addr
}

// freePort is httpserver's trick, repeated here because the two packages
// cannot share a test helper: bind a port, let it go, hand back the address.
// If something takes it in between, the role fails to listen and returns the
// error, which the assertions below read.
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

// TestAPIRoleServesHealthzAndStopsCleanly runs the "api" role as a person
// would — through the root command, from argv — and checks the two things a
// role has to do: come up far enough to answer on its port, and go back down
// when its context ends, returning no error.
//
// It is a SMOKE test and deliberately shallow: what it covers is the wiring in
// cmd/babki (config, pool, migrations, module mounting, the insert-only River
// client, the listener) rather than any behaviour of the modules themselves,
// each of which is tested where it lives.
//
// THE OTHER TWO LONG-RUNNING ROLES ARE NOT HERE, and the reason is not that
// they matter less. "worker" and "all" call startJobClient, which registers
// four periodic jobs with RunOnStart against cbr.New and moex.New — whose base
// URLs are production ones with no configuration knob (see startJobClient's
// own comment) — so starting either role in a test makes real requests to
// cbr.ru and iss.moex.com within milliseconds. The queue's own startup is
// covered against stub providers instead, one layer down, in
// internal/platform/jobs.
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

// TestMigrateRoleAppliesTheSchemaAndExits covers the one role that has to work
// on a machine where nothing else does yet: it is what a first deployment runs
// before any secret has been provisioned, and it must apply the schema to an
// empty database and exit rather than block.
//
// NO ENCRYPTION KEY IS SET, on purpose — that is the role's whole promise (see
// setup's requireEncryptionKey), and a test that provided one would not notice
// the promise being broken.
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
