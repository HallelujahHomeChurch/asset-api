package httpapi

import (
	"context"
	"errors"
	"fmt"
	"hhc/asset-api/internal/assets"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebsiteContentReadinessExactCaller(t *testing.T) {
	h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "test-token", WorkloadAuthConfig{}, nil)
	// A missing dependency must fail closed, even in development mode.
	r := httptest.NewRequest("POST", "/priv/website/content-readiness", strings.NewReader(`{"items":[{"type":"bulletin_pdf","id":"asset-a","resourceId":"issue-a","locale":"en"}]}`))
	r.Header.Set("Dapr-Caller-App-Id", "hhc-web-api")
	r.Header.Set("dapr-api-token", "test-token")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("missing owner dependency: got %d", w.Code)
	}
}

type websiteReadinessStub struct {
	calls int
	err   error
}

func (s *websiteReadinessStub) WebsiteContentReadiness(_ context.Context, refs []assets.WebsiteContentRef) ([]assets.WebsiteContentDecision, error) {
	s.calls++
	return []assets.WebsiteContentDecision{{Ref: refs[0], Ready: true}}, s.err
}
func TestWebsiteContentReadinessClosedAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, caller, body string
		duplicate, dev     bool
		want               int
	}{
		{"valid", "hhc-web-api", `{"items":[{"type":"bulletin_pdf","id":"asset-a","resourceId":"issue-a","locale":"en"}]}`, false, false, 200},
		{"wrong", "account-api", `{}`, false, false, 403},
		{"missing", "", `{}`, false, false, 403},
		{"development fallback", "", `{}`, false, true, 403},
		{"duplicate", "hhc-web-api", `{}`, true, false, 403},
		{"unknown ref", "hhc-web-api", `{"items":[{"type":"anything","id":"a","resourceId":"b"}]}`, false, false, 400},
		{"empty", "hhc-web-api", `{"items":[]}`, false, false, 400},
		{"unknown field", "hhc-web-api", `{"items":[{"type":"recording_live","id":"a","resourceId":"b","actorId":"c"}]}`, false, false, 400},
		{"forbidden locale", "hhc-web-api", `{"items":[{"type":"recording_live","id":"a","resourceId":"b","locale":"en"}]}`, false, false, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &websiteReadinessStub{}
			h := New(nil, nil, nil, true, "test-token", WorkloadAuthConfig{}, nil).WithWebsiteContentReadiness(store)
			r := httptest.NewRequest("POST", "/priv/website/content-readiness", strings.NewReader(tc.body))
			if tc.caller != "" {
				r.Header.Set("Dapr-Caller-App-Id", tc.caller)
			}
			if tc.duplicate {
				r.Header.Add("Dapr-Caller-App-Id", tc.caller)
			}
			if tc.dev {
				r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
			}
			r.Header.Set("dapr-api-token", "test-token")
			w := httptest.NewRecorder()
			h.Routes().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("got %d %s", w.Code, w.Body.String())
			}
			if tc.want != 200 && store.calls != 0 {
				t.Fatal("unauthorized store access")
			}
			if strings.Contains(w.Body.String(), "objects") || strings.Contains(w.Body.String(), "credential") {
				t.Fatal("capability leaked")
			}
		})
	}
	refs := make([]assets.WebsiteContentRef, 101)
	for i := range refs {
		refs[i] = assets.WebsiteContentRef{Type: "recording_live", ID: fmt.Sprint(i), ResourceID: "recording"}
	}
	if err := assets.ValidateWebsiteContentRefs(refs); err == nil {
		t.Fatal("unbounded refs")
	}
}

func TestWebsiteContentReadinessDependencyFailureIsUnavailable(t *testing.T) {
	store := &websiteReadinessStub{err: errors.New("database unavailable")}
	h := New(nil, nil, nil, false, "test-token", WorkloadAuthConfig{}, nil).WithWebsiteContentReadiness(store)
	r := httptest.NewRequest("POST", "/priv/website/content-readiness", strings.NewReader(`{"items":[{"type":"bulletin_pdf","id":"asset","resourceId":"issue","locale":"en"}]}`))
	r.Header.Set("Dapr-Caller-App-Id", "hhc-web-api")
	r.Header.Set("dapr-api-token", "test-token")
	w := httptest.NewRecorder()
	h.Routes().ServeHTTP(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), `"ready":false`) {
		t.Fatalf("outage became deny %d %s", w.Code, w.Body.String())
	}
}
