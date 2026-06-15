package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerDispatchesRegisteredRoute(t *testing.T) {
	server := NewServer()
	server.RegisterRoutes([]Route{{
		Method:      http.MethodGet,
		Path:        "/hello/{name}",
		OperationID: "getHello",
		Handler: func(request *http.Request) (any, error) {
			return "hello", nil
		},
	}})

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/hello/alice", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != "hello\n" {
		t.Fatalf("body = %q, want hello newline", got)
	}
}

func TestServerMapsHandlerError(t *testing.T) {
	server := NewServer()
	server.Handle(http.MethodGet, "/fail", func(request *http.Request) (any, error) {
		return nil, errors.New("secret boom")
	})

	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/fail", nil))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "secret boom") {
		t.Fatalf("body leaked handler error: %q", recorder.Body.String())
	}
}
