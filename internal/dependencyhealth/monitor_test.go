package dependencyhealth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	_ "github.com/jackc/pgx/v5/stdlib"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDependencyOutcomesAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		want   string
	}{
		{"healthy", nil, 204, "ok"}, {"status", nil, 503, "unavailable"},
		{"deadline", context.DeadlineExceeded, 0, "deadline"},
		{"cancelled", context.Canceled, 0, "cancelled"},
		{"transport", errors.New("secret-dsn-token"), 0, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&b, nil))
			client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "127.0.0.1:3500" || r.URL.Path != "/v1.0/healthz/outbound" || r.Header.Get("dapr-api-token") != "secret-dsn-token" {
					t.Fatal("incorrect probe")
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > 2*time.Second {
					t.Fatal("unbounded probe")
				}
				if tc.err != nil {
					return nil, tc.err
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader("private-response")), Header: make(http.Header)}, nil
			})}
			checkDapr(context.Background(), client, "3500", "secret-dsn-token", logger)
			s := b.String()
			if !strings.Contains(s, `"outcome":"`+tc.want+`"`) || !strings.Contains(s, `"dependency":"dapr_outbound"`) {
				t.Fatalf("unexpected event %s", s)
			}
			for _, secret := range []string{"secret-dsn-token", "private-response", "127.0.0.1"} {
				if strings.Contains(s, secret) {
					t.Fatal("private data in log")
				}
			}
		})
	}
}

func TestDBFailureIsIndependentAndSanitized(t *testing.T) {
	db, err := sql.Open("pgx", "host=127.0.0.1 password=secret-dsn-token dbname=unused")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	var b bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&b, nil))
	checkDB(context.Background(), db, logger)
	if !strings.Contains(b.String(), `"phase":"acquire"`) || !strings.Contains(b.String(), `"outcome":"unavailable"`) || strings.Contains(b.String(), "secret-dsn-token") {
		t.Fatal(b.String())
	}
	b.Reset()
	checkDB(context.Background(), nil, logger)
	if !strings.Contains(b.String(), `"outcome":"not_configured"`) {
		t.Fatal(b.String())
	}
}

func TestMonitoringCancellationAndInvalidPort(t *testing.T) {
	var b bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&b, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop := Start(ctx, nil, "3500", "secret", logger)
	stop()
	if b.Len() != 0 {
		t.Fatal("cancelled monitor made checks")
	}
	client := &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { t.Fatal("invalid port caused request"); return nil, nil })}
	for _, port := range []string{"", "0", "65536", "secret/path"} {
		checkDapr(context.Background(), client, port, "secret", logger)
	}
	if strings.Contains(b.String(), "secret") {
		t.Fatal("port leaked")
	}
}

func TestDaprCheckCancelsInFlight(t *testing.T) {
	var b bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&b, nil))
	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		cancel()
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	checkDapr(ctx, client, "3500", "", logger)
	if !strings.Contains(b.String(), `"outcome":"cancelled"`) {
		t.Fatal(b.String())
	}
}
