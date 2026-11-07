package jobidentity

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestEffectiveLoginCatalogGuard(t *testing.T) {
	dsn := os.Getenv("JOB_IDENTITY_TEST_DSN")
	if dsn == "" {
		t.Skip("requires isolated job_identity_test database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.Database != "job_identity_test" || (config.Host != "127.0.0.1" && config.Host != "localhost") {
		t.Fatal("requires disposable loopback job_identity_test database")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	role := fmt.Sprintf("job_guard_%d", time.Now().UnixNano())
	if _, err = admin.Exec(`CREATE ROLE ` + role + ` LOGIN CONNECTION LIMIT 1 PASSWORD 'fixture-only'`); err != nil {
		t.Fatal("create fixture role failed")
	}
	defer func() { _, _ = admin.Exec(`DROP ROLE ` + role) }()
	login := *config
	login.User = role
	login.Password = "fixture-only"
	// ConnString retains the original DSN, so construct the fixture connection explicitly.
	job, err := sql.Open("pgx", fmt.Sprintf("host=%s port=%d user=%s password=fixture-only dbname=job_identity_test sslmode=disable", login.Host, login.Port, role))
	if err != nil {
		t.Fatal(err)
	}
	defer job.Close()
	ctx := context.Background()
	if Check(ctx, job, role, 1) != nil {
		t.Fatal("bounded nonprivileged login rejected")
	}
	if Check(ctx, job, "wrong_role", 1) == nil {
		t.Fatal("wrong role accepted")
	}
	for _, change := range []string{"CONNECTION LIMIT -1", "CONNECTION LIMIT 2", "CONNECTION LIMIT 1 CREATEROLE", "NOCREATEROLE CREATEDB", "NOCREATEDB REPLICATION", "NOREPLICATION BYPASSRLS", "NOBYPASSRLS SUPERUSER"} {
		if _, err = admin.Exec(`ALTER ROLE ` + role + ` ` + change); err != nil {
			t.Fatal("fixture alteration failed")
		}
		if Check(ctx, job, role, 1) == nil {
			t.Fatalf("unsafe role accepted: %s", change)
		}
	}
	if _, err = admin.Exec(`ALTER ROLE ` + role + ` NOSUPERUSER`); err != nil {
		t.Fatal(err)
	}
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `SET ROLE `+role); err != nil {
		t.Fatal(err)
	}
	if Check(ctx, conn, role, 1) == nil {
		t.Fatal("switched effective role accepted")
	}
	_, _ = conn.ExecContext(ctx, `RESET ROLE`)
}
