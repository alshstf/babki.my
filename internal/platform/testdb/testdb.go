// Package testdb hands every test its own PostgreSQL database.
//
// One container per test binary, one database per test. The container starts
// lazily on the first call from the package and is shared by every test in it,
// so no package has to remember to write a TestMain of its own — a new package
// gets the sharing for free just by calling New. Teardown is left to the
// testcontainers reaper (Ryuk), which removes the container when the test
// process goes away: that covers a clean finish, a panic, a `go test` timeout
// and a Ctrl-C alike, none of which a TestMain would survive.
//
// Isolation is per database, not per schema and not per truncation: a test
// cannot see, lock or corrupt another test's rows, because its rows live in a
// database no other test is connected to. Migrations run once per package into
// a template database, and each test database is a copy of that template, so
// the schema costs one migration run per package instead of one per test.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"babki.my/babki/internal/platform/db"
)

const (
	image    = "postgres:17-alpine"
	dbUser   = "babki"
	dbSecret = "babki"

	// adminDatabase is the database this helper connects to in order to create
	// and drop the per-test ones. Nothing is ever stored in it.
	adminDatabase = "babki_admin"

	// templateDatabase carries the migrated schema. Every database handed to a
	// test is a copy of it.
	templateDatabase = "babki_template"

	startupTimeout = 90 * time.Second
)

// New returns a pool to a private, freshly migrated database. The database is
// dropped and the pool closed when the test ends. If Docker is unavailable the
// test is skipped; if Docker is there but the database cannot be provisioned,
// the test fails with a message that says so in as many words.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return open(t, templateDatabase)
}

// NewEmpty returns a pool to a private database with no schema in it at all —
// for tests of the migrations themselves, which need somewhere to run from
// scratch. Every other test wants New.
func NewEmpty(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return open(t, "")
}

func open(t *testing.T, template string) *pgxpool.Pool {
	t.Helper()
	srv := shared(t)
	ctx := context.Background()

	name := fmt.Sprintf("test_%04d", srv.seq.Add(1))
	if err := srv.createDatabase(ctx, name, template); err != nil {
		infraFatal(t, err)
	}
	t.Cleanup(func() {
		if err := srv.dropDatabase(name); err != nil {
			t.Logf("testdb: could not drop %s (harmless, the container is thrown away anyway): %v", name, err)
		}
	})

	pool, err := srv.connect(ctx, name)
	if err != nil {
		infraFatal(t, err)
	}
	// Cleanups run last-registered-first, so the pool is closed before the
	// database it points at is dropped.
	t.Cleanup(pool.Close)
	return pool
}

// server is the single Postgres container of this test binary.
type server struct {
	base  *url.URL // connection URL; Path names the database
	admin *pgxpool.Pool
	seq   atomic.Uint64

	// mu serializes CREATE/DROP DATABASE. Tests in this repository run
	// sequentially within a package, but nothing here should break the day one
	// of them calls t.Parallel.
	mu sync.Mutex
}

type startup struct {
	srv *server
	err error
	// noDocker separates "there is no Docker on this machine", which is a
	// reason to skip, from "Docker is here and something went wrong", which is
	// a reason to shout.
	noDocker bool
}

var (
	once sync.Once
	boot startup
)

// shared starts the container on first use and reports the same outcome to
// every later caller — including the failures, so a broken environment
// produces one diagnosis instead of one stalled test per test.
func shared(t *testing.T) *server {
	t.Helper()
	once.Do(func() { boot = start() })
	switch {
	case boot.noDocker:
		t.Skipf("testdb: skipping, Docker is not available: %v", boot.err)
	case boot.err != nil:
		infraFatal(t, boot.err)
	}
	return boot.srv
}

func start() startup {
	ctx := context.Background()

	provider, err := tc.ProviderDocker.GetProvider()
	if err != nil {
		return startup{err: err, noDocker: true}
	}
	defer func() { _ = provider.Close() }()
	if err := provider.Health(ctx); err != nil {
		return startup{err: err, noDocker: true}
	}

	ctr, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase(adminDatabase),
		tcpostgres.WithUsername(dbUser),
		tcpostgres.WithPassword(dbSecret),
		// The log line alone is not enough: it says the server inside the
		// container is up, not that the daemon has published the port. Waiting
		// for the mapped port as well is what keeps ConnectionString below from
		// failing with `port "5432/tcp" not found` on a loaded machine.
		tc.WithWaitStrategy(wait.ForAll(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		).WithStartupTimeoutDefault(startupTimeout)),
	)
	if err != nil {
		return startup{err: fmt.Errorf("start %s: %w", image, err)}
	}

	base, err := connectionURL(ctx, ctr)
	if err != nil {
		return startup{err: err}
	}
	srv := &server{base: base}

	admin, err := srv.connect(ctx, adminDatabase)
	if err != nil {
		return startup{err: err}
	}
	srv.admin = admin

	if err := srv.prepareTemplate(ctx); err != nil {
		return startup{err: err}
	}
	return startup{srv: srv}
}

// prepareTemplate builds the database every test database is copied from: an
// empty one, migrated once, then sealed off. PostgreSQL refuses to copy a
// database that somebody is connected to, so the migration pool is closed and
// the database is closed to new connections before it is ever used as a source.
func (s *server) prepareTemplate(ctx context.Context) error {
	if err := s.createDatabase(ctx, templateDatabase, ""); err != nil {
		return err
	}
	pool, err := s.connect(ctx, templateDatabase)
	if err != nil {
		return err
	}
	err = db.Migrate(ctx, pool)
	pool.Close()
	if err != nil {
		return fmt.Errorf("migrate template database: %w", err)
	}

	if _, err := s.admin.Exec(ctx,
		`ALTER DATABASE `+ident(templateDatabase)+` WITH ALLOW_CONNECTIONS false`); err != nil {
		return fmt.Errorf("seal template database: %w", err)
	}
	return s.waitIdle(ctx, templateDatabase)
}

// waitIdle blocks until no backend is left in the named database. pgxpool.Close
// returns once it has sent the terminations, not once the server has reaped the
// backends, and a straggler is enough to make CREATE DATABASE ... TEMPLATE fail.
func (s *server) waitIdle(ctx context.Context, database string) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		var n int
		err := s.admin.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`,
			database).Scan(&n)
		if err != nil {
			return fmt.Errorf("count backends of %s: %w", database, err)
		}
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d connection(s) still open on %s", n, database)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// createDatabase creates name, optionally as a copy of template. An empty
// template means the server default, i.e. an empty database.
func (s *server) createDatabase(ctx context.Context, name, template string) error {
	stmt := `CREATE DATABASE ` + ident(name)
	if template != "" {
		stmt += ` TEMPLATE ` + ident(template)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.admin.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// dropDatabase removes a finished test's database. FORCE takes care of whatever
// the test left connected — a River client, a leaked pool — so a test that ends
// untidily cannot hold the disk hostage for the rest of the run.
func (s *server) dropDatabase(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.admin.Exec(ctx, `DROP DATABASE IF EXISTS `+ident(name)+` WITH (FORCE)`)
	return err
}

// connect opens a pool on one database of the container. It uses db.PoolConfig
// (not pgxpool.New) so the test pool registers the same pgx type codecs as
// production — notably shopspring/decimal <-> NUMERIC — which store tests rely
// on when scanning into *decimal.Decimal fields.
func (s *server) connect(ctx context.Context, database string) (*pgxpool.Pool, error) {
	u := *s.base
	u.Path = "/" + database
	cfg, err := db.PoolConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("pool config for %s: %w", database, err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pool for %s: %w", database, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping %s: %w", database, err)
	}
	return pool, nil
}

// connectionURL asks the container where it listens, retrying the window in
// which the container is up but the daemon has not published the port mapping
// yet. That window is what used to surface as `port "5432/tcp" not found`; it
// is now hit at most once per package rather than once per test, and retried
// when it is hit.
func connectionURL(ctx context.Context, ctr *tcpostgres.PostgresContainer) (*url.URL, error) {
	var last error
	for attempt := range 30 {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		raw, err := ctr.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			last = err
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			last = err
			continue
		}
		return u, nil
	}
	return nil, fmt.Errorf("connection string: %w", last)
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// infraFatal fails the test with a banner nobody can mistake for a bug in the
// code under test. The old helper failed with a bare `connection string: ...`,
// which read exactly like a broken assertion.
func infraFatal(t *testing.T, err error) {
	t.Helper()
	t.Fatalf("\n"+
		"=== TEST INFRASTRUCTURE FAILURE — NOT A FAILURE OF THE CODE UNDER TEST ===\n"+
		"testdb could not give this test a database. Docker answered, so this is\n"+
		"the test environment failing, not an assertion.\n"+
		"cause: %v\n"+
		"==========================================================================",
		err)
}
