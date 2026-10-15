package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestDatabaseInventoryScopeAndReadOnly(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	databaseName := fmt.Sprintf("asset_dsr_inventory_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE ` + databaseName); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(`DROP DATABASE ` + databaseName + ` WITH (FORCE)`)
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = databaseName
	db := stdlib.OpenDB(*config)
	defer db.Close()
	ctx := context.Background()
	// The suite uses a dedicated, disposable database; no production URL is read.
	for _, statement := range []string{
		`CREATE TABLE assets (id text PRIMARY KEY, namespace text, owner_service text, owner_type text, object_key text, size_bytes bigint, deleted_at timestamptz, purged_at timestamptz)`,
		`CREATE TABLE asset_grants (id text PRIMARY KEY, asset_id text REFERENCES assets(id), expires_at timestamptz, revoked_at timestamptz)`,
		`CREATE TABLE upload_sessions (id text PRIMARY KEY, asset_id text REFERENCES assets(id), status text, expires_at timestamptz, staging_object_key text)`,
		`CREATE TABLE asset_scan_events (asset_id text REFERENCES assets(id))`,
		`CREATE TABLE asset_derivatives (asset_id text REFERENCES assets(id))`,
		`CREATE TABLE asset_scan_outbox (asset_id text REFERENCES assets(id))`,
		`CREATE TABLE asset_derivative_outbox (asset_id text REFERENCES assets(id))`,
		`CREATE TABLE asset_collection_items (asset_id text REFERENCES assets(id))`,
		`INSERT INTO assets VALUES ('export','account.dsr-export','account-api','user','assets/account.dsr-export/export',42,NULL,NULL),('supplement','account.dsr-supplement','account-api','user','assets/account.dsr-supplement/supplement',43,NULL,NULL),('avatar','account.avatar','account-api','user','assets/account.avatar/keep',44,NULL,NULL),('other-owner','account.dsr-export','other-api','user','other',45,NULL,NULL)`,
		`INSERT INTO asset_grants(id,asset_id) VALUES ('dsr-grant','export'),('ordinary-grant','avatar')`,
		`INSERT INTO upload_sessions(id,asset_id,status,staging_object_key) VALUES ('dsr-upload','supplement','completed','assets/account.dsr-supplement/staging'),('ordinary-upload','avatar','completed','keep')`,
		`INSERT INTO asset_scan_events VALUES ('export'),('avatar')`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	result, err := databaseInventory(ctx, db, databaseName)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 2 || len(result.Grants) != 1 || len(result.Uploads) != 1 || len(result.ForeignKeys) != 7 || len(result.Dependencies) != 7 {
		t.Fatalf("wrong scoped counts: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "avatar") || strings.Contains(string(encoded), "other-owner") || strings.Contains(string(encoded), "ordinary-grant") {
		t.Fatal("non-DSR row leaked")
	}
	var before, after int
	if err := db.QueryRow(`SELECT count(*) FROM assets`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	tx, err := beginInventoryTransaction(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var mode, isolation string
	if err := tx.QueryRowContext(ctx, `SHOW transaction_read_only`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if mode != "on" || isolation != "repeatable read" {
		t.Fatalf("unsafe transaction: %s %s", mode, isolation)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assets`); err == nil {
		t.Fatal("transaction allowed mutation")
	}
	_ = tx.Rollback()
	if err := db.QueryRow(`SELECT count(*) FROM assets`).Scan(&after); err != nil || before != after {
		t.Fatal("inventory changed data")
	}
	if _, err := databaseInventory(ctx, db, "different-database"); err == nil {
		t.Fatal("accepted wrong database")
	}
	if _, err := db.Exec(`CREATE TABLE unknown_dependency (asset_id text REFERENCES assets(id))`); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseInventory(ctx, db, databaseName); err == nil {
		t.Fatal("accepted unknown dependency")
	}
	if _, err := db.Exec(`DROP TABLE unknown_dependency`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE asset_grants ADD COLUMN related_asset_id text REFERENCES assets(id); UPDATE asset_grants SET related_asset_id='export' WHERE id='ordinary-grant'`); err != nil {
		t.Fatal(err)
	}
	if snapshot, err := databaseInventory(ctx, db, databaseName); err == nil || snapshot.Complete {
		t.Fatal("accepted unknown FK mapping on known table")
	}
	if _, err := db.Exec(`ALTER TABLE asset_grants DROP COLUMN related_asset_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO asset_grants(id,asset_id) SELECT 'overflow-' || n,'export' FROM generate_series(1,10001) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseInventory(ctx, db, databaseName); err == nil {
		t.Fatal("accepted oversized database inventory")
	}
	if _, err := db.Exec(`DELETE FROM asset_grants WHERE id LIKE 'overflow-%'`); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := databaseInventory(cancelled, db, databaseName); err == nil {
		t.Fatal("accepted cancelled inventory")
	}
	if _, err := db.Exec(`ALTER TABLE assets RENAME COLUMN namespace TO hidden_namespace`); err != nil {
		t.Fatal(err)
	}
	if _, err := databaseInventory(ctx, db, databaseName); err == nil {
		t.Fatal("accepted incomplete schema")
	}
}

func TestDatabaseTargetFence(t *testing.T) {
	good := "postgres://asset_runtime:unused@hhc-pg.postgres.database.azure.com:5432/asset?sslmode=require"
	for _, mode := range []string{"require", "verify-ca", "verify-full"} {
		config, err := inventoryDatabaseConfig(strings.Replace(good, "sslmode=require", "sslmode="+mode, 1))
		if err != nil {
			t.Fatal(err)
		}
		if config.TLSConfig.InsecureSkipVerify || config.TLSConfig.ServerName != "hhc-pg.postgres.database.azure.com" {
			t.Fatalf("unauthenticated TLS for %s", mode)
		}
	}
	db, err := openInventoryDatabase(good)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, dsn := range []string{"", "not a DSN", strings.Replace(good, "hhc-pg.postgres.database.azure.com", "localhost", 1), strings.Replace(good, "/asset?", "/account?", 1), strings.Replace(good, ":5432/", ":5433/", 1), strings.Replace(good, "sslmode=require", "sslmode=disable", 1), strings.Replace(good, "sslmode=require", "sslmode=prefer", 1)} {
		if db, err := openInventoryDatabase(dsn); err == nil {
			db.Close()
			t.Fatal("accepted wrong database target")
		}
	}
}
