package runtimehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSSESendsEvents(t *testing.T) {
	handler := SSE(func(request *http.Request, stream *SSEStream) error {
		return stream.Send(SSEEvent{ID: "1", Event: "message", Data: "hello"})
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected content type: %q", recorder.Header().Get("Content-Type"))
	}
	if got := recorder.Body.String(); got != "id: 1\nevent: message\ndata: hello\n\n" {
		t.Fatalf("unexpected SSE body: %q", got)
	}
}

func TestServerHandleSSERegistersRouteContext(t *testing.T) {
	server := NewServer()
	server.HandleSSE(http.MethodGet, "/events", "watchEvents", func(request *http.Request, stream *SSEStream) error {
		info, ok := RouteInfoFromContext(request.Context())
		if !ok || info.OperationID != "watchEvents" || !info.Features["stream"] {
			t.Fatalf("unexpected route info: %#v ok=%v", info, ok)
		}
		return stream.Send(SSEEvent{Data: "ready"})
	}, map[string]bool{"stream": true})

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/events", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "data: ready\n\n" {
		t.Fatalf("unexpected SSE response: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
