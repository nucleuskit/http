package runtimehttp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nucleuscontext "github.com/nucleuskit/core/context"
	"github.com/nucleuskit/core/inbound"
)

func TestNewInboundRequestFromHTTPPreservesRouteBodyAndMetadata(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/users/42?debug=true", strings.NewReader(`{"name":"annie"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("traceparent", "trace-http")
	request.Header.Set("X-Tenant-Id", "tenant-http")
	request = request.WithContext(nucleuscontext.WithRequestID(request.Context(), "request-http"))

	got, err := NewInboundRequest(request)
	if err != nil {
		t.Fatal(err)
	}

	if got.Kind != inbound.KindHTTP {
		t.Fatalf("expected HTTP kind, got %q", got.Kind)
	}
	if got.Route.Method != http.MethodPost || got.Route.Path != "/users/42" {
		t.Fatalf("unexpected route: %#v", got.Route)
	}
	if got.Body.ContentType != "application/json" || string(got.Body.Bytes) != `{"name":"annie"}` {
		t.Fatalf("unexpected body: %#v", got.Body)
	}
	if got.TraceID() != "trace-http" || got.RequestID() != "request-http" || got.Tenant() != "tenant-http" {
		t.Fatalf("metadata did not propagate: %#v", got.Metadata)
	}
	if rest, err := io.ReadAll(request.Body); err != nil || string(rest) != `{"name":"annie"}` {
		t.Fatalf("expected helper to restore request body, got %q err=%v", string(rest), err)
	}
}

func TestNewInboundRequestUsesRegisteredRouteContext(t *testing.T) {
	var got inbound.Request
	server := NewServer()
	server.RegisterRoutes([]Route{{
		Method:      http.MethodGet,
		Path:        "/users/{id}",
		OperationID: "getUser",
		Handler: func(request *http.Request) (any, error) {
			var err error
			got, err = NewInboundRequest(request)
			return map[string]string{"id": request.PathValue("id")}, err
		},
	}})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	request.Header.Set("X-Tenant-Id", "tenant-http")
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	if got.Route.Method != http.MethodGet || got.Route.Path != "/users/{id}" || got.Route.Operation != "getUser" {
		t.Fatalf("expected registered route context, got %#v", got.Route)
	}
	if got.Metadata.Get("http.endpoint") != "GET /users/{id}" {
		t.Fatalf("expected endpoint metadata, got %#v", got.Metadata)
	}
	if got.Metadata.Get("http.operation") != "getUser" {
		t.Fatalf("expected operation metadata, got %#v", got.Metadata)
	}
	if got.Metadata.Get("http.path") != "/users/42" {
		t.Fatalf("expected actual path metadata, got %#v", got.Metadata)
	}
	if got.Tenant() != "tenant-http" {
		t.Fatalf("expected tenant header metadata, got %#v", got.Metadata)
	}
}
