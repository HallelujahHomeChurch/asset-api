package meetingclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestListSyncWindows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/priv/meeting-sync-windows" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Dapr-Caller-App-Id") != "" || r.Header.Get("dapr-api-token") != "" {
			t.Fatalf("request=%s headers=%v", r.URL.String(), r.Header)
		}
		if r.URL.Query().Get("from") != "2026-09-02T00:00:00Z" || r.URL.Query().Get("to") != "2026-09-02T01:00:00Z" {
			t.Fatalf("query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"startsAt":"2026-09-02T00:15:00Z","endsAt":"2026-09-02T00:45:00Z"}]}`))
	}))
	defer server.Close()

	client := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token", nil })
	windows, err := client.ListSyncWindows(context.Background(), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC))
	if err != nil || len(windows) != 1 || windows[0].StartsAt.Minute() != 15 || windows[0].EndsAt.Minute() != 45 {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
}

func TestListSyncWindowsRejectsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", http.StatusBadGateway) }))
	defer server.Close()
	client := New(server.URL, server.Client(), func(context.Context) (string, error) { return "token", nil })
	if _, err := client.ListSyncWindows(context.Background(), time.Now(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expected error")
	}
}

func TestListSyncWindowsHonorsHTTPTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := New(server.URL, &http.Client{Timeout: time.Millisecond}, func(context.Context) (string, error) { return "token", nil })
	if _, err := client.ListSyncWindows(context.Background(), time.Now(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("expected timeout")
	}
}
