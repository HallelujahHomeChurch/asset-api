package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type databaseSnapshot struct {
	Name             string            `json:"name"`
	User             string            `json:"user"`
	ObservationStart time.Time         `json:"observationStart"`
	ObservationEnd   time.Time         `json:"observationEnd"`
	Complete         bool              `json:"complete"`
	Assets           []json.RawMessage `json:"assets"`
	Grants           []json.RawMessage `json:"grants"`
	Uploads          []json.RawMessage `json:"uploads"`
	ForeignKeys      []json.RawMessage `json:"foreignKeys"`
	Dependencies     []json.RawMessage `json:"dependencies"`
}

func openInventoryDatabase(dsn string) (*sql.DB, error) {
	config, err := inventoryDatabaseConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	return db, nil
}

func inventoryDatabaseConfig(dsn string) (*pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig(dsn)
	if err != nil || config.Host != "hhc-pg.postgres.database.azure.com" || config.Port != 5432 || config.Database != "asset" || config.TLSConfig == nil || len(config.Fallbacks) != 0 {
		return nil, errors.New("exact TLS production database target required")
	}
	// Resident require/verify-ca settings must also authenticate this exact server.
	config.TLSConfig = config.TLSConfig.Clone()
	config.TLSConfig.InsecureSkipVerify = false
	config.TLSConfig.ServerName = "hhc-pg.postgres.database.azure.com"
	return config, nil
}

func beginInventoryTransaction(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, errors.New("read-only database snapshot unavailable")
	}
	for _, statement := range []string{`SET LOCAL statement_timeout = '15s'`, `SET LOCAL lock_timeout = '2s'`, `SET LOCAL idle_in_transaction_session_timeout = '30s'`, `SET LOCAL TIME ZONE 'UTC'`, `SET LOCAL search_path = public, pg_catalog`} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			_ = tx.Rollback()
			return nil, errors.New("database snapshot limits unavailable")
		}
	}
	return tx, nil
}

func databaseInventory(ctx context.Context, db *sql.DB, expectedDatabase string) (databaseSnapshot, error) {
	result := databaseSnapshot{ObservationStart: time.Now().UTC()}
	tx, err := beginInventoryTransaction(ctx, db)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT current_database(),current_user`).Scan(&result.Name, &result.User); err != nil || result.Name != expectedDatabase {
		return result, errors.New("unexpected database identity")
	}
	const scope = ` WITH scoped AS (SELECT id FROM public.assets WHERE owner_service='account-api' AND namespace IN ('account.dsr-export','account.dsr-supplement')) `
	queries := []struct {
		sql    string
		output *[]json.RawMessage
	}{
		{`SELECT row_to_json(r) FROM (SELECT id,namespace,owner_service,owner_type,object_key,size_bytes,deleted_at,purged_at FROM public.assets WHERE owner_service='account-api' AND namespace IN ('account.dsr-export','account.dsr-supplement') ORDER BY id LIMIT 10001) r`, &result.Assets},
		{scope + `SELECT row_to_json(r) FROM (SELECT g.id,g.asset_id,g.expires_at,g.revoked_at FROM public.asset_grants g JOIN scoped a ON a.id=g.asset_id ORDER BY g.id LIMIT 10001) r`, &result.Grants},
		{scope + `SELECT row_to_json(r) FROM (SELECT u.id,u.asset_id,u.status,u.expires_at,u.staging_object_key FROM public.upload_sessions u JOIN scoped a ON a.id=u.asset_id ORDER BY u.id LIMIT 10001) r`, &result.Uploads},
		{`SELECT row_to_json(r) FROM (
		SELECT c.conname,n.nspname AS source_schema,t.relname AS source,pg_get_constraintdef(c.oid) AS definition,
		ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(attnum,position) JOIN pg_catalog.pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.attnum ORDER BY k.position) AS source_columns,
		ARRAY(SELECT a.attname FROM unnest(c.confkey) WITH ORDINALITY k(attnum,position) JOIN pg_catalog.pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.attnum ORDER BY k.position) AS target_columns
		FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_class t ON t.oid=c.conrelid JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
		WHERE c.contype='f' AND c.confrelid='public.assets'::regclass ORDER BY source_schema,source,conname LIMIT 10001) r`, &result.ForeignKeys},
		{scope + `SELECT row_to_json(r) FROM (
		SELECT 'asset_collection_items' AS source,count(*) AS rows FROM public.asset_collection_items d JOIN scoped a ON a.id=d.asset_id
		UNION ALL SELECT 'asset_derivative_outbox',count(*) FROM public.asset_derivative_outbox d JOIN scoped a ON a.id=d.asset_id
		UNION ALL SELECT 'asset_derivatives',count(*) FROM public.asset_derivatives d JOIN scoped a ON a.id=d.asset_id
		UNION ALL SELECT 'asset_grants',count(*) FROM public.asset_grants d JOIN scoped a ON a.id=d.asset_id
		UNION ALL SELECT 'asset_scan_events',count(*) FROM public.asset_scan_events d JOIN scoped a ON a.id=d.asset_id
		UNION ALL SELECT 'asset_scan_outbox',count(*) FROM public.asset_scan_outbox d JOIN scoped a ON a.id=d.asset_id
		UNION ALL SELECT 'upload_sessions',count(*) FROM public.upload_sessions d JOIN scoped a ON a.id=d.asset_id
		ORDER BY source) r`, &result.Dependencies},
	}
	count, size := 0, 0
	for _, query := range queries {
		*query.output = []json.RawMessage{}
		rows, err := tx.QueryContext(ctx, query.sql)
		if err != nil {
			return result, errors.New("database inventory query unavailable")
		}
		for rows.Next() {
			var record json.RawMessage
			if err := rows.Scan(&record); err != nil {
				rows.Close()
				return result, errors.New("database inventory row unavailable")
			}
			count++
			size += len(record)
			if count > maxObjects || size > maxBytes || !json.Valid(record) {
				rows.Close()
				return result, errors.New("database inventory bound or encoding failure")
			}
			*query.output = append(*query.output, record)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, errors.New("database inventory incomplete")
		}
	}
	known := map[string]bool{"asset_collection_items": true, "asset_derivative_outbox": true, "asset_derivatives": true, "asset_grants": true, "asset_scan_events": true, "asset_scan_outbox": true, "upload_sessions": true}
	for _, row := range result.ForeignKeys {
		var key struct {
			Source        string   `json:"source"`
			SourceSchema  string   `json:"source_schema"`
			SourceColumns []string `json:"source_columns"`
			TargetColumns []string `json:"target_columns"`
		}
		if json.Unmarshal(row, &key) != nil || key.SourceSchema != "public" || !known[key.Source] || len(key.SourceColumns) != 1 || key.SourceColumns[0] != "asset_id" || len(key.TargetColumns) != 1 || key.TargetColumns[0] != "id" {
			return result, errors.New("unknown database asset dependency")
		}
	}
	if err := tx.Commit(); err != nil {
		return result, errors.New("database snapshot completion failed")
	}
	result.ObservationEnd = time.Now().UTC()
	result.Complete = true
	return result, nil
}
