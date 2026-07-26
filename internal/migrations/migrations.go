package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed sql/*.sql
var files embed.FS

func Run(ctx context.Context, db *sql.DB) error {
	connection, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('hhc_asset_api_migrations'))`); err != nil {
		return err
	}
	defer connection.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext('hhc_asset_api_migrations'))`)
	if _, err := connection.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, checksum text NOT NULL DEFAULT '', applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	if _, err := connection.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum text NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	entries, err := fs.Glob(files, "sql/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)
	for _, name := range entries {
		contents, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(contents)
		checksum := hex.EncodeToString(sum[:])
		var storedChecksum string
		err = connection.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, name).Scan(&storedChecksum)
		if err == nil {
			if storedChecksum == "" {
				if _, err := connection.ExecContext(ctx, `UPDATE schema_migrations SET checksum=$2 WHERE version=$1 AND checksum=''`, name, checksum); err != nil {
					return err
				}
			} else if storedChecksum != checksum {
				return fmt.Errorf("migration %s checksum mismatch", name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tx, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(contents)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, name, checksum)
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
