package azure

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/log"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

func TestSDKResponseErrorEscapesRequestPath(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://fixture.invalid/container/blob%0Ainjected?sig=synthetic-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"X-Ms-Error-Code": {"AuthorizationFailure"}},
		Request:    req,
		Body:       io.NopCloser(strings.NewReader("")),
	}
	err = runtime.NewResponseError(response)
	message := err.Error()
	if !strings.Contains(message, "/container/blob%0Ainjected") || strings.Contains(message, "blob\ninjected") {
		t.Fatalf("request path was not escaped: %q", message)
	}
	if strings.Contains(message, "synthetic-secret") {
		t.Fatal("response error exposed a query value")
	}
}

type failingSDKTransport struct {
	requestURL string
}

func (transport *failingSDKTransport) Do(req *http.Request) (*http.Response, error) {
	transport.requestURL = req.URL.String()
	return nil, fmt.Errorf("synthetic transport failure for %s", req.URL)
}

func TestSDKResponseLogRedactsTransportErrorQuery(t *testing.T) {
	// The SDK listener is global; this test must remain sequential.
	var responseLog strings.Builder
	log.SetListener(func(event log.Event, message string) {
		if event == log.EventResponse {
			responseLog.WriteString(message)
		}
	})
	t.Cleanup(func() { log.SetListener(nil) })
	transport := &failingSDKTransport{}
	pipeline := runtime.NewPipeline("asset-sdk-regression", "v1.0.0", runtime.PipelineOptions{}, &azcore.ClientOptions{
		Transport: transport,
		Retry:     policy.RetryOptions{MaxRetries: -1},
	})
	const endpoint = "https://fixture.invalid/container/blob?api-version=synthetic-version&sig=synthetic-secret"
	req, err := runtime.NewRequest(context.Background(), http.MethodGet, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.Do(req); err == nil {
		t.Fatal("expected transport failure")
	}
	if transport.requestURL != endpoint {
		t.Fatalf("outbound URL changed: %q", transport.requestURL)
	}
	message := responseLog.String()
	if !strings.Contains(message, "synthetic transport failure") || !strings.Contains(message, "api-version=synthetic-version") {
		t.Fatalf("missing response error diagnostics: %q", message)
	}
	if strings.Contains(message, "synthetic-secret") || !strings.Contains(message, "sig=REDACTED") {
		t.Fatalf("response log did not redact the query value: %q", message)
	}
}
