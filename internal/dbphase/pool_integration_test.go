package dbphase

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type event struct {
	phase   string
	elapsed time.Duration
	failed  bool
}
type events struct {
	sync.Mutex
	values []event
}

func (e *events) report(_ context.Context, p string, d time.Duration, f bool) {
	if !f && d < 250*time.Millisecond {
		return
	}
	e.Lock()
	defer e.Unlock()
	e.values = append(e.values, event{p, d, f})
}
func (e *events) take() []event {
	e.Lock()
	defer e.Unlock()
	values := e.values
	e.values = nil
	return values
}
func config(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	dsn := os.Getenv("DB_PHASE_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable loopback DB_PHASE_TEST_DSN")
	}
	return loopbackConfig(t, dsn, "db_phase_test")
}

func loopbackConfig(t *testing.T, dsn, database string) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || (database != "" && cfg.Database != database) ||
		(cfg.Host != "localhost" && cfg.Host != "127.0.0.1" && cfg.Host != "::1") {
		t.Fatal("requires a disposable loopback test database")
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.Host != cfg.Host {
			t.Fatal("requires one loopback host")
		}
	}
	return cfg
}

type priorKey struct{}
type priorTracer struct{ queries, connects, ended atomic.Int32 }

func (p *priorTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	p.queries.Add(1)
	return context.WithValue(ctx, priorKey{}, true)
}
func (p *priorTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(priorKey{}) == true {
		p.ended.Add(1)
	}
}
func (p *priorTracer) TraceConnectStart(ctx context.Context, _ pgx.TraceConnectStartData) context.Context {
	p.connects.Add(1)
	return ctx
}
func (p *priorTracer) TraceConnectEnd(context.Context, pgx.TraceConnectEndData) {}
func TestRealQueryRowsAndPoolWaitBoundaries(t *testing.T) {
	cfg := config(t)
	previous := new(priorTracer)
	cfg.Tracer = previous
	var observed events
	db := Open(*cfg, observed.report)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if len(observed.take()) != 0 {
		t.Fatal("fast path emitted slow/failure")
	}
	if previous.queries.Load() == 0 || previous.connects.Load() == 0 || previous.ended.Load() == 0 {
		t.Fatal("existing optional tracer lost")
	}
	if cfg.Tracer != previous {
		t.Fatal("caller configuration mutated")
	}
	if _, err := db.ExecContext(t.Context(), "SELECT pg_sleep(0.3)"); err != nil {
		t.Fatal(err)
	}
	got := observed.take()
	if len(got) != 1 || got[0].phase != "db_query_lifecycle" || got[0].failed {
		t.Fatalf("slow SQL misattributed: %+v", got)
	}
	rows, err := db.QueryContext(t.Context(), "SELECT generate_series(1,2)")
	if err != nil {
		t.Fatal(err)
	}
	if len(observed.take()) != 0 {
		t.Fatal("query ended before consumption")
	}
	time.Sleep(300 * time.Millisecond)
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	got = observed.take()
	if len(got) != 1 || got[0].phase != "db_query_lifecycle" {
		t.Fatalf("rows lifecycle missing: %+v", got)
	}
	if _, err := db.ExecContext(t.Context(), "SELECT 'secret sql'::integer"); err == nil {
		t.Fatal("expected fixture SQL error")
	}
	got = observed.take()
	if len(got) != 1 || !got[0].failed {
		t.Fatalf("query failure missing: %+v", got)
	}
	held, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := db.Stats()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err = db.Conn(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if db.Stats().WaitCount-before.WaitCount != 1 || len(observed.take()) != 0 {
		t.Fatal("global pool wait attributed to query/connect")
	}
	held.Close()
	ctx, cancelQuery := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancelQuery()
	_, err = db.ExecContext(ctx, "SELECT pg_sleep(1)")
	if err == nil {
		t.Fatal("cancelled query succeeded")
	}
	got = observed.take()
	if len(got) != 1 || !got[0].failed || got[0].phase != "db_query_lifecycle" {
		t.Fatalf("query cancellation missing: %+v", got)
	}
}
func TestRealConnectDelayAndApplicationName(t *testing.T) {
	cfg := config(t)
	cfg.RuntimeParams["application_name"] = "original"
	t.Setenv("CONTAINER_APP_REVISION", "fixture-revision")
	dial := cfg.DialFunc
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		time.Sleep(300 * time.Millisecond)
		return dial(ctx, network, address)
	}
	var observed events
	db := Open(*cfg, observed.report)
	defer db.Close()
	var name string
	if err := db.QueryRowContext(t.Context(), "SHOW application_name").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "fixture-revision" || cfg.RuntimeParams["application_name"] != "original" {
		t.Fatal("revision identity/config mutation")
	}
	got := observed.take()
	if len(got) != 1 || got[0].phase != "db_connect" {
		t.Fatalf("connect misattributed: %+v", got)
	}
}

type gatedConn struct {
	net.Conn
	blocked *atomic.Bool
	closed  chan struct{}
	once    sync.Once
}

func (c *gatedConn) Read(p []byte) (int, error) {
	if c.blocked.Load() {
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		case <-time.After(100 * time.Millisecond):
		}
	}
	return c.Conn.Read(p)
}
func (c *gatedConn) Close() error { c.once.Do(func() { close(c.closed) }); return c.Conn.Close() }
func TestRealResetCancellation(t *testing.T) {
	cfg := config(t)
	var blocked atomic.Bool
	cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &gatedConn{Conn: c, blocked: &blocked, closed: make(chan struct{})}, nil
	}
	var observed events
	db := sql.OpenDB(connector{Connector: stdlib.GetConnector(*cfg, stdlib.OptionShouldPing(func(context.Context, stdlib.ShouldPingParams) bool { return true })), report: observed.report})
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	held, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	held.Close()
	observed.take()
	blocked.Store(true)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = db.Conn(ctx)
	if err == nil {
		t.Fatal("blocked reset succeeded")
	}
	got := observed.take()
	found := false
	for _, e := range got {
		if e.phase == "db_reset" && e.failed {
			found = true
		}
		if e.phase == "db_query_lifecycle" {
			t.Fatal("reset classified as query")
		}
	}
	if !found {
		t.Fatalf("reset failure missing: %+v", got)
	}
}

func TestRevisionTagPreservesLocalAndBoundsLongNames(t *testing.T) {
	for _, revision := range []string{"", strings.Repeat("revision-", 10), strings.Repeat("界", 40)} {
		t.Setenv("CONTAINER_APP_REVISION", revision)
		cfg := config(t)
		cfg.RuntimeParams["application_name"] = "local-name"
		db := Open(*cfg, func(context.Context, string, time.Duration, bool) {})
		var name string
		err := db.QueryRowContext(t.Context(), "SHOW application_name").Scan(&name)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if revision == "" && name != "local-name" {
			t.Fatal("local application name overwritten")
		}
		if len(name) > 63 || (revision != "" && name == "local-name") {
			t.Fatalf("unbounded/missing revision tag: %q bytes=%d", name, len(name))
		}
	}
}

// Drop writes on the warmed socket only. Reads retain the real TCP deadlines
// installed by pgx, and a replacement connection reaches the same database.
type stalledSocket struct {
	net.Conn
	stalled atomic.Bool
	onStall func()
}

func (c *stalledSocket) Write(p []byte) (int, error) {
	if c.stalled.Load() {
		if c.onStall != nil {
			c.onStall()
		}
		return len(p), nil
	}
	return c.Conn.Write(p)
}

func reuseConfig(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	if os.Getenv("DB_PHASE_TEST_DSN") != "" {
		return config(t)
	}
	// Run in the existing CI PostgreSQL lane as well as the standalone phase fixture.
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" && os.Getenv("ASSET_ALLOW_DESTRUCTIVE_TEST") == "1" {
		return loopbackConfig(t, dsn, "")
	}
	t.Skip("requires disposable loopback PostgreSQL")
	return nil
}

func TestRealStalledPoolReuse(t *testing.T) {
	for _, tc := range []struct {
		name              string
		callerBudget      time.Duration
		refuseReplacement bool
		cancelOnPing      bool
		wantReset         time.Duration
		wantErr           error
	}{
		{name: "replace", callerBudget: 6 * time.Second, wantReset: 2 * time.Second},
		{name: "shorter caller", callerBudget: 200 * time.Millisecond, wantReset: 200 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "caller cancellation", callerBudget: 6 * time.Second, cancelOnPing: true, wantErr: context.Canceled},
		{name: "unavailable replacement", callerBudget: 6 * time.Second, refuseReplacement: true, wantReset: 2 * time.Second, wantErr: net.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reuseConfig(t)
			caller, cancelCaller := context.WithCancel(t.Context())
			defer cancelCaller()
			dial := cfg.DialFunc
			var first atomic.Pointer[stalledSocket]
			cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				if first.Load() != nil && tc.refuseReplacement {
					return nil, net.ErrClosed
				}
				c, err := dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				socket := &stalledSocket{Conn: c}
				if tc.cancelOnPing {
					socket.onStall = cancelCaller
				}
				first.CompareAndSwap(nil, socket)
				return socket, nil
			}
			var observed events
			db := Open(*cfg, observed.report)
			defer db.Close()
			db.SetMaxOpenConns(1)
			var oldPID, newPID int
			if err := db.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&oldPID); err != nil {
				t.Fatal(err)
			}
			// Use pgx's default idle ping policy.
			time.Sleep(1100 * time.Millisecond)
			first.Load().stalled.Store(true)
			ctx, cancel := context.WithTimeout(caller, tc.callerBudget)
			defer cancel()
			err := db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&newPID)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("query error = %v, want %v", err, tc.wantErr)
			}
			if err == nil && newPID == oldPID {
				t.Fatal("stalled backend was reused")
			}
			for _, e := range observed.take() {
				if e.phase == "db_reset" && e.failed {
					target := tc.wantReset
					if e.elapsed < target-100*time.Millisecond || e.elapsed > target+time.Second {
						t.Fatalf("reset took %v, want approximately %v", e.elapsed, target)
					}
					return
				}
			}
			t.Fatal("failed reset missing")
		})
	}
}

func TestHealthyReusedTransactionKeepsCallerBudget(t *testing.T) {
	cfg := reuseConfig(t)
	db := Open(*cfg, func(context.Context, string, time.Duration, bool) {})
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT pg_sleep(2.2)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
