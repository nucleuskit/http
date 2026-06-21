package runtimehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	caphealth "github.com/nucleuskit/nucleus/cap/health"
	"github.com/nucleuskit/nucleus/core/response"
)

func TestGovernanceIsExplicitAndUsesEnvelope(t *testing.T) {
	server := NewServer()

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, GovernanceLivenessPath, nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected governance liveness to be absent by default, got %d", recorder.Code)
	}

	server.RegisterGovernance(
		WithGovernanceLiveness(),
		WithGovernanceMetadata(func(*http.Request) (map[string]any, error) {
			return map[string]any{"service": "orders"}, nil
		}),
		WithGovernanceEndpoint(GovernanceEndpoint{
			Method: http.MethodGet,
			Path:   "/admin/debug",
			Handler: func(*http.Request) (any, error) {
				return map[string]string{"mode": "debug"}, nil
			},
		}),
	)

	assertEnvelopeData(t, server, GovernanceLivenessPath, "status", "ok")
	assertEnvelopeData(t, server, GovernanceMetadataPath, "service", "orders")
	assertEnvelopeData(t, server, "/admin/debug", "mode", "debug")
}

func TestGovernanceReadinessReusesHealthReporters(t *testing.T) {
	server := NewServer()
	server.RegisterGovernance(WithGovernanceReadiness(caphealth.StaticReport(caphealth.Report{
		Capability: "sql",
		Status:     caphealth.StatusReady,
		Message:    "ready",
	})))

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, GovernanceReadinessPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	readiness := envelope.Data.(map[string]any)
	if readiness["ready"] != true {
		t.Fatalf("expected readiness ready=true, got %#v", readiness)
	}
}

func assertEnvelopeData(t *testing.T, server *Server, path string, key string, want any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200 for %s, got %d", path, recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	if data[key] != want {
		t.Fatalf("expected %s=%v for %s, got %#v", key, want, path, data[key])
	}
}
