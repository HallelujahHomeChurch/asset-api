package httpapi

import (
	"database/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessDoesNotQueryUnavailableDatabase(t *testing.T) {
	db, err := sql.Open("pgx", "host=127.0.0.1 dbname=unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	h := &Handler{db: db}
	w := httptest.NewRecorder()
	h.ready(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("dependency outage removed initialized AP: %d", w.Code)
	}
}
func TestReadinessRejectsUninitializedApplication(t *testing.T) {
	w := httptest.NewRecorder()
	(&Handler{}).ready(w, httptest.NewRequest(http.MethodGet, "/ready", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
}
