package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseRange(t *testing.T) {
	value, partial, err := parseRange("bytes=10-19")
	if err != nil || !partial || value.Offset != 10 || value.Count != 10 {
		t.Fatalf("range=%+v partial=%v err=%v", value, partial, err)
	}
	value, partial, err = parseRange("bytes=10-")
	if err != nil || !partial || value.Offset != 10 || value.Count != 0 {
		t.Fatalf("open range=%+v partial=%v err=%v", value, partial, err)
	}
}

func TestParseRangeRejectsSuffixAndMultipleRanges(t *testing.T) {
	for _, input := range []string{"bytes=-10", "bytes=0-1,3-4", "items=0-1"} {
		if _, _, err := parseRange(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

func TestProductionCallerUsesDaprIdentity(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("Dapr-Caller-App-Id", "account-api")
	request.Header.Set("X-Internal-Caller-App-Id", "hhc-web-api")

	if caller := callerFromRequest(request, false); caller != "account-api" {
		t.Fatalf("caller = %q", caller)
	}
}

func TestProductionCallerRejectsDevelopmentHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/priv/assets/upload-sessions", nil)
	request.Header.Set("X-Internal-Caller-App-Id", "account-api")

	if caller := callerFromRequest(request, false); caller != "" {
		t.Fatalf("caller = %q", caller)
	}
	if caller := callerFromRequest(request, true); caller != "account-api" {
		t.Fatalf("development caller = %q", caller)
	}
}

func TestLocalUploadCORSAllowsAdminDevelopmentOrigin(t *testing.T) {
	nextCalled := false
	handler := localUploadCORS(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodOptions, "/dev/uploads/token", nil)
	request.Header.Set("Origin", "http://127.0.0.1:5175")
	request.Header.Set("Access-Control-Request-Method", http.MethodPut)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || nextCalled {
		t.Fatalf("status=%d next=%v", response.Code, nextCalled)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5175" {
		t.Fatalf("allow origin=%q", response.Header().Get("Access-Control-Allow-Origin"))
	}
}
