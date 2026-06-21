package runtimehttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPProfExtensionDisabledByDefault(t *testing.T) {
	server := NewServer()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected pprof to be disabled by default with 404, got %d", recorder.Code)
	}
}

func TestPProfExtensionRegistersDebugRoutesWhenEnabled(t *testing.T) {
	server := NewServer(WithPProf())

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected pprof index status 200, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Types of profiles available") {
		t.Fatalf("expected pprof index body, got %q", recorder.Body.String())
	}
}

func TestOpenAPIExtensionDisabledByDefault(t *testing.T) {
	server := NewServer()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected OpenAPI serving to be disabled by default with 404, got %d", recorder.Code)
	}
}

func TestOpenAPIExtensionServesStaticSpecAndSwaggerWhenEnabled(t *testing.T) {
	spec := []byte("openapi: 3.0.3\ninfo:\n  title: test\n  version: v1\npaths: {}\n")
	server := NewServer(WithOpenAPI(OpenAPIOptions{Spec: spec}))

	specRecorder := httptest.NewRecorder()
	specRequest := httptest.NewRequest(http.MethodGet, "/openapi.yaml", nil)
	server.Handler().ServeHTTP(specRecorder, specRequest)

	if specRecorder.Code != http.StatusOK {
		t.Fatalf("expected OpenAPI spec status 200, got %d", specRecorder.Code)
	}
	if got := specRecorder.Header().Get("Content-Type"); got != "application/yaml; charset=utf-8" {
		t.Fatalf("expected YAML content type, got %q", got)
	}
	if specRecorder.Body.String() != string(spec) {
		t.Fatalf("expected static spec body, got %q", specRecorder.Body.String())
	}

	swaggerRecorder := httptest.NewRecorder()
	swaggerRequest := httptest.NewRequest(http.MethodGet, "/swagger/", nil)
	server.Handler().ServeHTTP(swaggerRecorder, swaggerRequest)

	if swaggerRecorder.Code != http.StatusOK {
		t.Fatalf("expected swagger status 200, got %d", swaggerRecorder.Code)
	}
	body := swaggerRecorder.Body.String()
	if !strings.Contains(body, "SwaggerUIBundle") || !strings.Contains(body, "/openapi.yaml") {
		t.Fatalf("expected swagger HTML to reference static OpenAPI path, got %q", body)
	}
}
