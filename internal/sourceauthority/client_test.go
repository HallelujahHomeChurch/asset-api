package sourceauthority

import (
	"context"
	"encoding/json"
	"fmt"
	"hhc/asset-api/internal/assets"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthorityClientUsesExactTargetsAndSeparateCredentials(t *testing.T) {
	gateToken, accountToken := strings.Repeat("a", 64), strings.Repeat("b", 64)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "" || r.Header.Get("X-HHC-Caller-App-Id") != "" {
			t.Errorf("unexpected identity %s", r.Method)
		}
		switch r.URL.Path {
		case "/priv/source/check":
			if r.Header.Get("X-HHC-Harness-Service-Token") != gateToken || r.Header.Get("X-HHC-Harness-Service-Caller") != "asset-api" || r.Header.Get("X-HHC-Account-Service-Token") != "" {
				t.Error("wrong source credential")
			}
			fmt.Fprint(w, `{"version":1,"state":"active","policyRevision":4,"revocationRevision":1}`)
		case "/priv/account/v1/current-subject":
			if r.Header.Get("X-HHC-Account-Service-Token") != accountToken || r.Header.Get("X-HHC-Account-Service-Caller") != "asset-api" || r.Header.Get("X-HHC-Harness-Service-Token") != "" {
				t.Error("wrong Account credential")
			}
			fmt.Fprint(w, `{"version":1,"active":true,"userId":"reader","roleIds":["role"]}`)
		default:
			t.Error("unexpected route")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, gateToken, server.URL, accountToken)
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport
	if result, err := client.CheckSource(context.Background(), assets.SourceAccessRequest{Version: 1, Mode: "control"}); err != nil || result.State != "active" {
		t.Fatalf("source %+v %v", result, err)
	}
	if subject, err := client.CurrentSubject(context.Background(), "reader"); err != nil || subject.UserID != "reader" || len(subject.RoleIDs) != 1 {
		t.Fatalf("subject %+v %v", subject, err)
	}
	for _, origin := range []string{"http://insecure.test", "https://user:secret@host.test", "https://host.test/path", "https://host.test?token=secret", "https://host.test#fragment"} {
		if _, err := New(origin, gateToken, server.URL, accountToken); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe origin error %v", err)
		}
	}
}

func TestAuthorityClientFailsClosedOnMalformedOversizeAndRedirect(t *testing.T) {
	for _, body := range []string{`{"version":2,"state":"active","policyRevision":1,"revocationRevision":1}`, `{"version":1,"state":"active","policyRevision":1,"revocationRevision":1,"content":"private"}`, `{"version":1,"state":"active","policyRevision":1,"revocationRevision":1} {}`, strings.Repeat("x", 9000)} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		client, err := New(server.URL, strings.Repeat("a", 64), server.URL, strings.Repeat("b", 64))
		if err != nil {
			t.Fatal(err)
		}
		client.http.Transport = server.Client().Transport
		if _, err := client.CheckSource(context.Background(), assets.SourceAccessRequest{Version: 1, Mode: "control"}); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatalf("untrusted response accepted %v", err)
		}
		server.Close()
	}
	hits := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, _ := New(server.URL, strings.Repeat("a", 64), server.URL, strings.Repeat("b", 64))
	client.http.Transport = server.Client().Transport
	if _, err := client.CheckSource(context.Background(), assets.SourceAccessRequest{Version: 1, Mode: "control"}); err == nil || hits != 1 {
		t.Fatalf("redirect followed hits=%d err=%v", hits, err)
	}
}

func TestBatchAuthorityBoundsAndCorrelatesDecisions(t *testing.T) {
	requests := make([]assets.SourceAccessRequest, 100)
	for i := range requests {
		requests[i] = assets.SourceAccessRequest{Version: 1, Mode: "control", CollectionID: fmt.Sprint(i)}
	}
	for _, fault := range []string{"", "missing", "reordered", "unknown", "extra", "version"} {
		t.Run(fault, func(t *testing.T) {
			hits := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				if r.URL.Path != "/priv/source/check-batch" || r.Header.Get("X-HHC-Harness-Service-Caller") != "asset-api" {
					t.Error("wrong batch boundary")
				}
				var input struct {
					Version  int `json:"version"`
					Requests []struct {
						ID      string                     `json:"id"`
						Request assets.SourceAccessRequest `json:"request"`
					} `json:"requests"`
				}
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Fatal(err)
				}
				if len(input.Requests) != 100 || input.Version != 1 {
					t.Error("wrong batch size")
				}
				results := make([]map[string]any, 100)
				for i, v := range input.Requests {
					results[i] = map[string]any{"id": v.ID, "decision": assets.SourceDecision{Version: 1, RevocationRevision: 1, State: "active", PolicyRevision: 4}}
				}
				version := 1
				switch fault {
				case "missing":
					results = results[:99]
				case "reordered":
					results[0], results[1] = results[1], results[0]
				case "unknown":
					results[0]["decision"] = assets.SourceDecision{Version: 1, RevocationRevision: 1, State: "unknown", PolicyRevision: 4}
				case "extra":
					results[0]["content"] = "private"
				case "version":
					version = 2
				}
				json.NewEncoder(w).Encode(map[string]any{"version": version, "decisions": results})
			}))
			defer server.Close()
			client, _ := New(server.URL, strings.Repeat("a", 64), server.URL, strings.Repeat("b", 64))
			client.http.Transport = server.Client().Transport
			result, err := client.CheckSources(context.Background(), requests)
			if fault == "" {
				if err != nil || len(result) != 100 {
					t.Fatalf("batch result %d %v", len(result), err)
				}
			} else if err == nil {
				t.Fatal("malformed batch allowed")
			}
			for _, invalid := range [][]assets.SourceAccessRequest{nil, make([]assets.SourceAccessRequest, 101)} {
				if _, err := client.CheckSources(context.Background(), invalid); err == nil {
					t.Fatal("unbounded input")
				}
			}
			if hits != 1 {
				t.Fatalf("HTTP calls=%d", hits)
			}
		})
	}
}

func TestBatchAuthorityHonorsCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	client, _ := New(server.URL, strings.Repeat("a", 64), server.URL, strings.Repeat("b", 64))
	client.http.Transport = server.Client().Transport
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	defer close(release)
	start := time.Now()
	_, err := client.CheckSources(ctx, []assets.SourceAccessRequest{{Version: 1, Mode: "control"}})
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("batch ignored cancellation")
	}
}
