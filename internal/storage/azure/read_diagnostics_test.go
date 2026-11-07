package azure

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/telemetry"
)

func TestAzureDownloadRecordsRealSDKRetriesAndBody(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("x-ms-error-code", "ServerBusy")
			w.WriteHeader(503)
			io.WriteString(w, `<Error><Code>ServerBusy</Code></Error>`)
			return
		}
		io.WriteString(w, "bytes")
	}))
	defer server.Close()
	options := diagnosticClientOptions()
	options.Retry = policy.RetryOptions{MaxRetries: 1, RetryDelay: time.Millisecond, MaxRetryDelay: time.Millisecond}
	client, err := azblob.NewClientWithNoCredential(server.URL, options)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, container: "private-container"}
	telemetry.Server(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		download, err := store.Open(r.Context(), "private-object", assets.ByteRange{}, "")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(download.Body)
		if err != nil || string(data) != "bytes" {
			t.Fatal(err)
		}
		if err := download.Body.Close(); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(200)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/assets/public/synthetic", nil))
	if calls != 2 {
		t.Fatalf("SDK retry changed %d", calls)
	}
	for _, want := range []string{`"phase":"storage_attempt","count":2`, `"phase":"storage_retry","count":1`, `"phase":"storage_body","count":1`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("missing %s: %s", want, logs.String())
		}
	}
	for _, secret := range []string{"private-object", "private-container", server.URL} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("storage details leaked")
		}
	}
}
