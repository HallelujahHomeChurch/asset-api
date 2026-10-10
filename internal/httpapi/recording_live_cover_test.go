package httpapi

import (
	"context"
	"fmt"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/storage/r2"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiveCoverRoutesFailClosed(t *testing.T) {
	h := New(nil, nil, map[string]bool{"hhc-web-api": true, "account-api": true}, true, "", WorkloadAuthConfig{}, nil).Routes()
	for _, tc := range []struct {
		method, path, caller string
		want                 int
	}{
		{"POST", "/priv/recording-live-covers/defaults/uploads", "account-api", 403},
		{"POST", "/priv/recording-live-covers/defaults/uploads", "hhc-web-api", 503},
		{"GET", "/priv/recording-live-covers/defaults/uploads/one", "hhc-web-api", 503},
		{"GET", "/priv/recording-live-covers/defaults/uploads/one/content", "account-api", 403},
		{"POST", "/priv/recording-live-covers/capture/auto", "account-api", 403},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("X-Internal-Caller-App-Id", tc.caller)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.want)
		}
	}
}

type promotionProbe struct {
	RecordingLiveCoverStore
	called bool
}

func (p *promotionProbe) Promote(context.Context, string, string, string) (assets.RecordingCover, error) {
	p.called = true
	return assets.RecordingCover{ID: "cover", Kind: "live-auto", State: "pending"}, nil
}

type coverProbe struct{ RecordingCoverStore }

func TestLiveCoverPromotionEmptyChunkedBody(t *testing.T) {
	for _, length := range []int64{0, -1} {
		t.Run(fmt.Sprint(length), func(t *testing.T) {
			probe := &promotionProbe{}
			h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCovers(&coverProbe{}, &r2.Store{}).WithRecordingLiveCovers(probe)
			r := httptest.NewRequest("POST", "/promote?recordingId=recording", nil)
			r.Body = io.NopCloser(strings.NewReader(""))
			r.ContentLength = length
			r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
			r.SetPathValue("scope", strings.Repeat("a", 32))
			r.SetPathValue("uploadID", "cover")
			w := httptest.NewRecorder()
			h.promoteLiveCover(w, r)
			if w.Code != 202 || !probe.called {
				t.Fatalf("empty promotion rejected: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLiveCoverPromotionRejectsInvalidChunkedPayload(t *testing.T) {
	for _, body := range []string{"{", `{"unknown":true}`, `{"targetCaptureId":"a"} {}`, strings.Repeat(" ", 2049)} {
		probe := &promotionProbe{}
		h := New(nil, nil, map[string]bool{"hhc-web-api": true}, true, "", WorkloadAuthConfig{}, nil).WithRecordingCovers(&coverProbe{}, &r2.Store{}).WithRecordingLiveCovers(probe)
		r := httptest.NewRequest("POST", "/promote?recordingId=recording", strings.NewReader(body))
		r.ContentLength = -1
		r.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")
		w := httptest.NewRecorder()
		h.promoteLiveCover(w, r)
		if w.Code != 400 || probe.called {
			t.Fatalf("invalid promotion accepted: %d", w.Code)
		}
	}
}
