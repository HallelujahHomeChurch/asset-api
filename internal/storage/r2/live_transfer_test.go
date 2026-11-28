package r2

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

type liveTransferResponder struct{}

func (liveTransferResponder) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 503, Header: http.Header{"Cf-Ray": {"123-SEA"}, "X-Amz-Request-Id": {"invalid credential value"}}}, nil
}

func TestLiveTransferCorrelatesProviderResponse(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	ctx := WithLiveAttempt(t.Context(), "capture", "claim", "480p", "freeze", 924)
	request, err := http.NewRequestWithContext(ctx, "PUT", "https://example.invalid/private-object?token=sensitive", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (storageHTTPObserver{liveTransferResponder{}}).Do(request); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"sequence":924`, `"status":503`, `"cf_ray":"123-SEA"`, `"provider_request_id":""`, `"request_written":false`} {
		if !strings.Contains(logs.String(), field) {
			t.Fatalf("missing diagnostic %s", field)
		}
	}
	for _, private := range []string{"private-object", "sensitive", "invalid credential value"} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("unexpected private diagnostic")
		}
	}
}
