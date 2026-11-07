// Package dbphase observes runtime pgx phases without retaining SQL or arguments.
package dbphase

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/stdlib"
)

type Reporter func(context.Context, string, time.Duration, bool)

func OpenURL(dsn string, report Reporter) (*sql.DB, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	return Open(*config, report), nil
}

// Open is runtime-only: migration Raw consumers must keep an unwrapped pool.
func Open(config pgx.ConnConfig, report Reporter) *sql.DB {
	config = *config.Copy()
	if revision := os.Getenv("CONTAINER_APP_REVISION"); revision != "" {
		if strings.IndexFunc(revision, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
			sum := sha256.Sum256([]byte(revision))
			revision = fmt.Sprintf("revision-%x", sum[:8])
		} else if len(revision) > 63 {
			sum := sha256.Sum256([]byte(revision))
			prefix := revision[:46]
			revision = fmt.Sprintf("%s-%x", prefix, sum[:8])
		}
		config.RuntimeParams["application_name"] = revision
	}
	tracer := queryTracer{report: report}
	if config.Tracer == nil {
		config.Tracer = tracer
	} else {
		config.Tracer = multitracer.New(config.Tracer, tracer)
	}
	return sql.OpenDB(connector{Connector: stdlib.GetConnector(config), report: report})
}

type connector struct {
	driver.Connector
	report Reporter
}

func (c connector) Connect(ctx context.Context) (driver.Conn, error) {
	started := time.Now()
	raw, err := c.Connector.Connect(ctx)
	c.report(ctx, "db_connect", time.Since(started), err != nil)
	if err != nil {
		return nil, err
	}
	return &conn{nativeConn: raw.(*stdlib.Conn), report: c.report}, nil
}

// Embedding retains pgx's optional interfaces; never unwrap migrations through this.
type nativeConn = stdlib.Conn

type conn struct {
	*nativeConn
	report Reporter
}

func (c *conn) ResetSession(ctx context.Context) error {
	started := time.Now()
	err := c.nativeConn.ResetSession(ctx)
	c.report(ctx, "db_reset", time.Since(started), err != nil)
	return err
}

type queryKey struct{}
type queryTracer struct{ report Reporter }

func (t queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryKey{}, time.Now())
}
func (t queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, ok := ctx.Value(queryKey{}).(time.Time)
	if !ok {
		return
	}
	// pgx ends this at rows closure: includes result consumption, not server-only SQL time.
	t.report(ctx, "db_query_lifecycle", time.Since(started), data.Err != nil)
}

var _ driver.ConnBeginTx = (*conn)(nil)
var _ driver.ConnPrepareContext = (*conn)(nil)
var _ driver.ExecerContext = (*conn)(nil)
var _ driver.QueryerContext = (*conn)(nil)
var _ driver.Pinger = (*conn)(nil)
var _ driver.SessionResetter = (*conn)(nil)
var _ driver.NamedValueChecker = (*conn)(nil)
