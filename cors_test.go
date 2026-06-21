package runtimehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCORSMiddlewareHandlesPreflight(t *testing.T) {
	handler := CORS(CORSOptions{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{http.MethodGet, http.MethodPost},
		AllowedHeaders: []string{"Authorization", "X-Trace-Id"},
		ExposeHeaders:  []string{"X-Result"},
		Credentials:    true,
		MaxAge:         10 * time.Minute,
	})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight should not call next handler")
	}))

	request := httptest.NewRequest(http.MethodOptions, "/widgets", nil)
	request.Header.Set("Origin", "https://app.example.com")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "Authorization, X-Trace-Id")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected preflight status 204, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Origin", "https://app.example.com")
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Methods", "GET, POST")
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Headers", "Authorization, X-Trace-Id")
	assertHeader(t, recorder.Header(), "Access-Control-Expose-Headers", "X-Result")
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Credentials", "true")
	assertHeader(t, recorder.Header(), "Access-Control-Max-Age", "600")
	assertHeader(t, recorder.Header(), "Vary", "Origin")
}

func TestCORSMiddlewareAddsHeadersToNormalResponse(t *testing.T) {
	handler := CORS(CORSOptions{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{http.MethodGet},
		AllowedHeaders: []string{"Content-Type"},
	})(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	}))

	request := httptest.NewRequest(http.MethodGet, "/widgets", nil)
	request.Header.Set("Origin", "https://client.example.com")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Origin", "*")
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Methods", "GET")
	assertHeader(t, recorder.Header(), "Access-Control-Allow-Headers", "Content-Type")
}

func TestCORSMiddlewareSkipsDisallowedOrigin(t *testing.T) {
	called := false
	handler := CORS(CORSOptions{AllowedOrigins: []string{"https://app.example.com"}})(
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			called = true
			writer.WriteHeader(http.StatusAccepted)
		}),
	)

	request := httptest.NewRequest(http.MethodGet, "/widgets", nil)
	request.Header.Set("Origin", "https://evil.example.com")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	if !called {
		t.Fatal("expected normal request to continue")
	}
	if recorder.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected CORS origin header: %q", recorder.Header().Get("Access-Control-Allow-Origin"))
	}
}

func assertHeader(t *testing.T, header http.Header, key string, want string) {
	t.Helper()
	if got := header.Get(key); got != want {
		t.Fatalf("expected %s %q, got %q", key, want, got)
	}
}
