package postgres

import (
	"errors"
	"testing"
)

func TestFinishRowsReturnsIterationError(t *testing.T) {
	want := errors.New("iteration failed")
	rows := &errorRowSet{err: want}

	err := finishRows(rows)

	if !errors.Is(err, want) || !rows.closed {
		t.Fatalf("err=%v closed=%v", err, rows.closed)
	}
}

type errorRowSet struct {
	err    error
	closed bool
}

func (r *errorRowSet) Err() error {
	return r.err
}

func (r *errorRowSet) Close() error {
	r.closed = true
	return nil
}
