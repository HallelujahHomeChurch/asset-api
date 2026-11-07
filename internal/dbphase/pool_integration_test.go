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
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil || cfg.Database != "db_phase_test" || (cfg.Host != "localhost" && cfg.Host != "127.0.0.1") {
		t.Fatal("requires disposable loopback db_phase_test")
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
