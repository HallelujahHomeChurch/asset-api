package migrations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// A native pgx transaction has no sql.Tx auto-rollback goroutine racing session
// unlock. All operations and bounded cleanup remain with the Conn owner.
type migrationTransaction struct {
	*sql.Conn
	tx   pgx.Tx
	ctx  context.Context
	done bool
}

func beginMigrationTransaction(ctx context.Context, conn *sql.Conn) (*migrationTransaction, error) {
	var tx pgx.Tx
	err := conn.Raw(func(raw any) error {
		native, ok := raw.(*stdlib.Conn)
		if !ok {
			return errors.New("session transaction requires pgx")
		}
		var err error
		tx, err = native.Conn().Begin(ctx)
		if err != nil {
			return errors.Join(err, driver.ErrBadConn)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &migrationTransaction{Conn: conn, tx: tx, ctx: ctx}, nil
}

func (tx *migrationTransaction) Commit() error {
	if tx.done {
		return sql.ErrTxDone
	}
	err := tx.Conn.Raw(func(any) error { return tx.tx.Commit(tx.ctx) })
	if err == nil {
		tx.done = true
		return nil
	}
	return errors.Join(err, tx.Rollback())
}

func (tx *migrationTransaction) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(tx.ctx), 5*time.Second)
	defer cancel()
	return tx.Conn.Raw(func(any) error {
		err := tx.tx.Rollback(cleanup)
		if errors.Is(err, pgx.ErrTxClosed) {
			return nil
		}
		if err != nil {
			return errors.Join(err, driver.ErrBadConn)
		}
		return nil
	})
}
