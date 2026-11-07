// Package jobidentity verifies the effective database job identity before work.
package jobidentity

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const query = `SELECT current_user::text, session_user::text, rolconnlimit,
 rolcanlogin, rolsuper, rolcreaterole, rolcreatedb, rolreplication, rolbypassrls
 FROM pg_catalog.pg_roles WHERE rolname = current_user`

var errRejected = errors.New("database job identity or connection budget rejected")

type identity struct {
	current, session                                           string
	limit                                                      int
	login, super, createRole, createDB, replication, bypassRLS bool
}

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Check reads catalog metadata only. Errors deliberately omit DSNs and role names.
func Check(ctx context.Context, db querier, expected string, maximum int) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var role identity
	if err := db.QueryRowContext(ctx, query).Scan(&role.current, &role.session, &role.limit, &role.login, &role.super, &role.createRole, &role.createDB, &role.replication, &role.bypassRLS); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRejected
	}
	return validate(role, expected, maximum)
}
func validate(role identity, expected string, maximum int) error {
	if expected == "" || maximum < 1 || role.current != expected || role.session != expected || role.limit < 1 || role.limit > maximum || !role.login || role.super || role.createRole || role.createDB || role.replication || role.bypassRLS {
		return errRejected
	}
	return nil
}
