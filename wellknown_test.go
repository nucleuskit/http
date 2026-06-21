package runtimehttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	caphealth "github.com/nucleuskit/nucleus/cap/health"
	"github.com/nucleuskit/nucleus/core/response"
)

func TestRegisterWellKnownServesNucleusDescriptionSubset(t *testing.T) {
	server := NewServer()
	server.RegisterWellKnown(func(*http.Request) (WellKnown, error) {
		return WellKnown{
			SchemaVersion: "1.0",
			Service: map[string]any{
				"name": "demo",
			},
			Capabilities: []string{"http"},
			Endpoints: []Endpoint{
				{Method: http.MethodGet, Path: "/healthz", OperationID: "getHealthz"},
			},
		}, nil
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/.well-known/nucleus.json", nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data, ok := envelope.Data.(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %#v", envelope.Data)
	}
	if data["schema_version"] != "1.0" {
		t.Fatalf("expected schema version 1.0, got %#v", data["schema_version"])
	}
}

func TestWellKnownWithReadinessAggregatesCapabilityReports(t *testing.T) {
	server := NewServer()
	server.RegisterWellKnown(func(request *http.Request) (WellKnown, error) {
		readiness, err := ReadinessFromRequest(request,
			caphealth.StaticReport(caphealth.Report{Capability: "sql", Status: caphealth.StatusReady}),
			caphealth.StaticReport(caphealth.Report{Capability: "redis", Status: caphealth.StatusDegraded, Message: "ping timeout"}),
		)
		if err != nil {
			return WellKnown{}, err
		}
		return WellKnown{
			SchemaVersion: "1.0",
			Capabilities:  []string{"sql", "redis"},
			Readiness:     readiness,
		}, nil
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, WellKnownPath, nil)
	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(map[string]any)
	readiness := data["readiness"].(map[string]any)
	if readiness["ready"] != false || readiness["status"] != "degraded" {
		t.Fatalf("unexpected readiness: %#v", readiness)
	}
	reports := readiness["reports"].([]any)
	if len(reports) != 2 {
		t.Fatalf("expected two capability reports, got %#v", reports)
	}
}
