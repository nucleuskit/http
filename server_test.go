package runtimehttp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	coreerrors "github.com/nucleuskit/nucleus/core/errors"
	"github.com/nucleuskit/nucleus/core/response"
)

func TestServerRegisterRoutesUsesPathValues(t *testing.T) {
	server := NewServer()
	server.RegisterRoutes([]Route{{
		Method:      http.MethodGet,
		Path:        "/users/{id}",
		OperationID: "getUser",
		Handler: func(request *http.Request) (any, error) {
			return map[string]string{"id": request.PathValue("id")}, nil
		},
	}})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	if data["id"] != "42" {
		t.Fatalf("expected path id 42, got %#v", data["id"])
	}
}

func TestServerMiddlewareRecoversAndAddsRequestID(t *testing.T) {
	server := NewServer(WithMiddleware(Recovery(), RequestID()))
	server.Handle(http.MethodGet, "/panic", func(*http.Request) (any, error) {
		panic("boom")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/panic", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", recorder.Code)
	}
	if recorder.Header().Get("X-Request-Id") == "" {
		t.Fatal("expected X-Request-Id header")
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != coreerrors.CodeInternal.Int() {
		t.Fatalf("expected internal code, got %d", envelope.Code)
	}
}

func TestServerMapsCodeErrorStatus(t *testing.T) {
	server := NewServer()
	server.Handle(http.MethodGet, "/bad", func(*http.Request) (any, error) {
		return nil, coreerrors.New(coreerrors.CodeInvalidArgument, "bad query")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/bad", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != coreerrors.CodeInvalidArgument.Int() || envelope.Message != "bad query" {
		t.Fatalf("unexpected envelope: %#v", envelope)
	}
}

func TestServerSupportsExplicitSuccessStatus(t *testing.T) {
	server := NewServer()
	server.Handle(http.MethodPost, "/items", func(*http.Request) (any, error) {
		return WithStatus(http.StatusCreated, map[string]string{"id": "item-1"}), nil
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/items", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	if data["id"] != "item-1" {
		t.Fatalf("expected response data to unwrap status payload, got %#v", data)
	}
}

func TestServerMapsUnknownErrorToInternalStatus(t *testing.T) {
	server := NewServer()
	server.Handle(http.MethodGet, "/bad", func(*http.Request) (any, error) {
		return nil, errors.New("plain error")
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/bad", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", recorder.Code)
	}
}

func TestResponseEnvelopeWithCustomEncoder(t *testing.T) {
	handler := ResponseEnvelopeWithEncoder(func(*http.Request) (any, error) {
		return "ok", nil
	}, ResponseEncoderFunc(func(writer http.ResponseWriter, request *http.Request, data any, err error) {
		writer.Header().Set("X-Encoded", data.(string))
		writer.WriteHeader(http.StatusAccepted)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/encoded", nil))

	if recorder.Code != http.StatusAccepted || recorder.Header().Get("X-Encoded") != "ok" {
		t.Fatalf("custom encoder did not run: status=%d header=%q", recorder.Code, recorder.Header().Get("X-Encoded"))
	}
}

func TestServerRoutesExposeMetadataAndFeatureSelector(t *testing.T) {
	selected := 0
	server := NewServer(WithMiddleware(Feature("beta", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			selected++
			writer.Header().Set("X-Beta", "on")
			next.ServeHTTP(writer, request)
		})
	})))
	server.RegisterRoutes([]Route{
		{
			Method:      http.MethodGet,
			Path:        "/stable",
			OperationID: "getStable",
			Handler:     func(*http.Request) (any, error) { return "stable", nil },
		},
		{
			Method:      http.MethodGet,
			Path:        "/beta",
			OperationID: "getBeta",
			Features:    map[string]bool{"beta": true},
			Handler: func(request *http.Request) (any, error) {
				if !FeatureEnabled(request.Context(), "beta") {
					t.Fatal("expected route feature in context")
				}
				return "beta", nil
			},
		},
	})

	if routes := server.Routes(); len(routes) != 2 || routes[1].OperationID != "getBeta" || !routes[1].Features["beta"] {
		t.Fatalf("unexpected routes: %#v", routes)
	}
	stable := httptest.NewRecorder()
	server.Handler().ServeHTTP(stable, httptest.NewRequest(http.MethodGet, "/stable", nil))
	if stable.Header().Get("X-Beta") != "" {
		t.Fatalf("stable route should not have selected beta middleware")
	}
	beta := httptest.NewRecorder()
	server.Handler().ServeHTTP(beta, httptest.NewRequest(http.MethodGet, "/beta", nil))
	if beta.Header().Get("X-Beta") != "on" || selected != 1 {
		t.Fatalf("expected beta middleware once, header=%q selected=%d", beta.Header().Get("X-Beta"), selected)
	}
}

func TestRouteGunzipAndMaxBytesMiddleware(t *testing.T) {
	server := NewServer()
	server.RegisterRoutes([]Route{{
		Method:   http.MethodPost,
		Path:     "/upload",
		Gunzip:   true,
		MaxBytes: 16,
		Handler: func(request *http.Request) (any, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			return map[string]string{"body": string(body)}, nil
		},
	}})

	body := gzipBytes(t, []byte("hello"))
	request := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	request.Header.Set("Content-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	if data["body"] != "hello" {
		t.Fatalf("expected decompressed body, got %#v", data["body"])
	}
}

func TestDefaultGunzipAndMaxBytesMiddleware(t *testing.T) {
	server := NewServer(WithMiddleware(DefaultMiddlewareChain(
		WithDefaultGunzip(true),
		WithDefaultMaxBytes(16),
	)...))
	server.Handle(http.MethodPost, "/upload", func(request *http.Request) (any, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		return map[string]string{"body": string(body)}, nil
	})

	body := gzipBytes(t, []byte("hello"))
	request := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body))
	request.Header.Set("Content-Encoding", "gzip")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	if data["body"] != "hello" {
		t.Fatalf("expected decompressed body, got %#v", data["body"])
	}
}

func TestDefaultMaxConnsMiddlewareRejectsOverflow(t *testing.T) {
	server := NewServer(WithMiddleware(DefaultMiddlewareChain(WithDefaultMaxConns(1))...))
	entered := make(chan struct{})
	release := make(chan struct{})
	server.Handle(http.MethodGet, "/hold", func(*http.Request) (any, error) {
		close(entered)
		<-release
		return "ok", nil
	})
	handler := server.Handler()

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/hold", nil))
		firstDone <- recorder
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter handler")
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/hold", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("expected overflow status 429, got %d body=%s", second.Code, second.Body.String())
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("expected first request status 200, got %d body=%s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish")
	}
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
