package runtimehttp

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

type SSEEvent struct {
	ID    string
	Event string
	Data  string
	Retry time.Duration
}

type SSEHandler func(*http.Request, *SSEStream) error

type SSEStream struct {
	writer  http.ResponseWriter
	flusher http.Flusher
}

func (s *Server) HandleSSE(method string, path string, operationID string, handler SSEHandler, features map[string]bool) {
	route := Route{Method: method, Path: path, OperationID: operationID, Features: features}
	registered := cloneRoute(route)
	s.recordRoute(registered)
	s.mux.Handle(routePattern(registered.Method, registered.Path), sseWithRoute(registered, handler))
}

func SSE(handler SSEHandler) http.Handler {
	return sseWithRoute(Route{}, handler)
}

func (s *SSEStream) Send(event SSEEvent) error {
	if s == nil || s.writer == nil {
		return nil
	}
	if event.ID != "" {
		if _, err := fmt.Fprintf(s.writer, "id: %s\n", cleanSSELine(event.ID)); err != nil {
			return err
		}
	}
	if event.Event != "" {
		if _, err := fmt.Fprintf(s.writer, "event: %s\n", cleanSSELine(event.Event)); err != nil {
			return err
		}
	}
	if event.Retry > 0 {
		if _, err := fmt.Fprintf(s.writer, "retry: %d\n", event.Retry.Milliseconds()); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(event.Data, "\n") {
		if _, err := fmt.Fprintf(s.writer, "data: %s\n", cleanSSELine(line)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(s.writer, "\n"); err != nil {
		return err
	}
	s.Flush()
	return nil
}

func (s *SSEStream) Flush() {
	if s != nil && s.flusher != nil {
		s.flusher.Flush()
	}
}

func sseWithRoute(route Route, handler SSEHandler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if route.Method != "" || route.Path != "" || route.OperationID != "" {
			request = request.WithContext(withRouteContext(request.Context(), route))
		}
		flusher, ok := writer.(http.Flusher)
		if !ok {
			http.Error(writer, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.Header().Set("Connection", "keep-alive")
		writer.WriteHeader(http.StatusOK)
		if handler == nil {
			return
		}
		if err := handler(request, &SSEStream{writer: writer, flusher: flusher}); err != nil {
			_, _ = fmt.Fprintf(writer, "event: error\ndata: %s\n\n", cleanSSELine(err.Error()))
			flusher.Flush()
		}
	})
}

func cleanSSELine(value string) string {
	value = strings.ReplaceAll(value, "\r", "")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}
