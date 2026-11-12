package sourceauthority

import (
	"context"
	"fmt"
	"hhc/asset-api/internal/assets"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			fmt.Fprint(w, `{"version":1,"state":"active","policyRevision":4}`)
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
	for _, body := range []string{`{"version":2,"state":"active","policyRevision":1}`, `{"version":1,"state":"active","policyRevision":1,"content":"private"}`, `{"version":1,"state":"active","policyRevision":1} {}`, strings.Repeat("x", 9000)} {
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
