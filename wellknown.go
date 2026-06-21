package runtimehttp

import (
	"net/http"

	caphealth "github.com/nucleuskit/cap/health"
)

const WellKnownPath = "/.well-known/nucleus.json"

const (
	GovernanceLivenessPath  = "/.well-known/nucleus/live"
	GovernanceReadinessPath = "/.well-known/nucleus/ready"
	GovernanceMetadataPath  = "/.well-known/nucleus/metadata"
)

type Endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id"`
}

type WellKnown struct {
	SchemaVersion string               `json:"schema_version"`
	Service       any                  `json:"service"`
	Capabilities  []string             `json:"capabilities"`
	Endpoints     []Endpoint           `json:"endpoints"`
	Readiness     *caphealth.Readiness `json:"readiness,omitempty"`
}

type WellKnownProvider func(*http.Request) (WellKnown, error)

type GovernanceEndpoint struct {
	Method      string
	Path        string
	OperationID string
	Handler     Handler
}

type GovernanceMetadataProvider func(*http.Request) (map[string]any, error)

type GovernanceOption func(*governanceOptions)

type governanceOptions struct {
	endpoints []GovernanceEndpoint
}

func (s *Server) RegisterWellKnown(provider WellKnownProvider) {
	s.Handle(http.MethodGet, WellKnownPath, func(request *http.Request) (any, error) {
		return provider(request)
	})
}

func (s *Server) RegisterGovernance(options ...GovernanceOption) {
	cfg := governanceOptions{}
	for _, option := range options {
		if option != nil {
			option(&cfg)
		}
	}
	for _, endpoint := range cfg.endpoints {
		method := endpoint.Method
		if method == "" {
			method = http.MethodGet
		}
		if endpoint.Path == "" || endpoint.Handler == nil {
			continue
		}
		s.Handle(method, endpoint.Path, endpoint.Handler)
	}
}

func WithGovernanceEndpoint(endpoint GovernanceEndpoint) GovernanceOption {
	return func(options *governanceOptions) {
		options.endpoints = append(options.endpoints, endpoint)
	}
}

func WithGovernanceLiveness() GovernanceOption {
	return WithGovernanceEndpoint(GovernanceEndpoint{
		Method: http.MethodGet,
		Path:   GovernanceLivenessPath,
		Handler: func(*http.Request) (any, error) {
			return map[string]string{"status": "ok"}, nil
		},
	})
}

func WithGovernanceMetadata(provider GovernanceMetadataProvider) GovernanceOption {
	return WithGovernanceEndpoint(GovernanceEndpoint{
		Method: http.MethodGet,
		Path:   GovernanceMetadataPath,
		Handler: func(request *http.Request) (any, error) {
			if provider == nil {
				return map[string]any{}, nil
			}
			return provider(request)
		},
	})
}

func WithGovernanceReadiness(reporters ...caphealth.Reporter) GovernanceOption {
	return WithGovernanceEndpoint(GovernanceEndpoint{
		Method: http.MethodGet,
		Path:   GovernanceReadinessPath,
		Handler: func(request *http.Request) (any, error) {
			return ReadinessFromRequest(request, reporters...)
		},
	})
}

func ReadinessFromRequest(request *http.Request, reporters ...caphealth.Reporter) (*caphealth.Readiness, error) {
	readiness, err := caphealth.Aggregate(request.Context(), reporters...)
	if err != nil {
		return nil, err
	}
	return &readiness, nil
}
