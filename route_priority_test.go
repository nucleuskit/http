package runtimehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	capsentinel "github.com/nucleuskit/nucleus/cap/sentinel"
)

func TestRoutePriorityIsClonedAndReadableFromContext(t *testing.T) {
	server := NewServer()
	route := Route{
		Method:   http.MethodGet,
		Path:     "/priority",
		Priority: 9,
		Handler: func(request *http.Request) (any, error) {
			priority, ok := RoutePriorityFromContext(request.Context())
			if !ok || priority != 9 {
				t.Fatalf("expected priority 9 in context, got %d ok=%v", priority, ok)
			}
			return "ok", nil
		},
	}
	server.HandleRoute(route)
	route.Priority = 1

	routes := server.Routes()
	if len(routes) != 1 || routes[0].Priority != 9 {
		t.Fatalf("expected cloned route priority 9, got %#v", routes)
	}
	routes[0].Priority = 2
	if got := server.Routes()[0].Priority; got != 9 {
		t.Fatalf("expected route clone mutation not to leak, got %d", got)
	}

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/priority", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSentinelResourceIncludesRoutePriorityWhenAvailable(t *testing.T) {
	breaker := &recordingPriorityBreaker{}
	server := NewServer(WithMiddleware(Sentinel(breaker, nil)))
	server.RegisterRoutes([]Route{{
		Method:   http.MethodGet,
		Path:     "/priority",
		Priority: 3,
		Handler:  func(*http.Request) (any, error) { return "ok", nil },
	}})

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/priority", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := sentinelAttribute(breaker.resource, "priority"); got != "3" {
		t.Fatalf("expected priority sentinel attribute 3, got %#v", got)
	}
}

type recordingPriorityBreaker struct {
	resource capsentinel.Resource
}

func (b *recordingPriorityBreaker) Allow(_ context.Context, resource capsentinel.Resource) (capsentinel.Guard, error) {
	b.resource = resource
	return nil, nil
}
