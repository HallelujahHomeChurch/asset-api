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

func TestCallerNamespacePolicy(t *testing.T) {
	tests := []struct {
		caller, namespace string
		allowed           bool
	}{
		{"hhc-web-api", "cms.weekly.pdf", true},
		{"hhc-web-api", "line.group.file", false},
		{"hhc-line-function-bot", "line.group.file", true},
		{"hhc-line-function-bot", "cms.page.image", false},
	}
	for _, test := range tests {
		if got := callerCanUseNamespace(test.caller, test.namespace); got != test.allowed {
			t.Fatalf("caller=%s namespace=%s allowed=%v", test.caller, test.namespace, got)
		}
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
