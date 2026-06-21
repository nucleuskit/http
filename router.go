package runtimehttp

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type Route struct {
	Method      string
	Path        string
	OperationID string
	Handler     Handler
	Timeout     time.Duration
	MaxBytes    int64
	MaxConns    int
	Gunzip      bool
	Priority    int
	Features    map[string]bool
	Middleware  []Middleware
}

type routePriorityContextKey struct{}

func (s *Server) RegisterRoutes(routes []Route) {
	for _, route := range routes {
		s.HandleRoute(route)
	}
}

func (s *Server) HandleRoute(route Route) {
	registered := cloneRoute(route)
	if registered.Handler == nil {
		registered.Handler = func(*http.Request) (any, error) {
			return nil, http.ErrNotSupported
		}
	}
	s.recordRoute(registered)

	handler := ResponseEnvelope(func(request *http.Request) (any, error) {
		ctx := withRouteContext(request.Context(), registered)
		ctx = withRoutePriorityContext(ctx, registered.Priority)
		return registered.Handler(request.WithContext(ctx))
	})
	middleware := routeMiddleware(registered)
	if len(middleware) > 0 {
		handler = Chain(handler, middleware...)
	}
	s.mux.Handle(routePattern(registered.Method, registered.Path), handler)
}

func (s *Server) routeContextHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if route, ok := matchRegisteredRoute(s.Routes(), request); ok {
			ctx := withRouteContext(request.Context(), route)
			ctx = withRoutePriorityContext(ctx, route.Priority)
			request = request.WithContext(ctx)
		}
		next.ServeHTTP(writer, request)
	})
}

func RoutePriorityFromContext(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	priority, ok := ctx.Value(routePriorityContextKey{}).(int)
	return priority, ok
}

func withRoutePriorityContext(ctx context.Context, priority int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if priority == 0 {
		return ctx
	}
	return context.WithValue(ctx, routePriorityContextKey{}, priority)
}

func (s *Server) recordRoute(route Route) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes = append(s.routes, route)
}

func matchRegisteredRoute(routes []Route, request *http.Request) (Route, bool) {
	for _, route := range routes {
		if !strings.EqualFold(route.Method, request.Method) {
			continue
		}
		if routePathMatches(route.Path, request.URL.Path) {
			return route, true
		}
	}
	return Route{}, false
}

func routePathMatches(pattern string, path string) bool {
	if pattern == path {
		return true
	}
	patternParts := splitRoutePath(pattern)
	pathParts := splitRoutePath(path)
	if len(patternParts) != len(pathParts) {
		return false
	}
	for i, part := range patternParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			continue
		}
		if part != pathParts[i] {
			return false
		}
	}
	return true
}

func splitRoutePath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func routeMiddleware(route Route) []Middleware {
	middleware := make([]Middleware, 0, len(route.Middleware)+4)
	if route.MaxConns > 0 {
		middleware = append(middleware, MaxConns(route.MaxConns))
	}
	if route.Gunzip {
		middleware = append(middleware, Gunzip())
	}
	if route.MaxBytes > 0 {
		middleware = append(middleware, MaxBytes(route.MaxBytes))
	}
	if route.Timeout > 0 {
		middleware = append(middleware, Timeout(route.Timeout))
	}
	middleware = append(middleware, route.Middleware...)
	return middleware
}

func cloneRoutes(routes []Route) []Route {
	cloned := make([]Route, len(routes))
	for i, route := range routes {
		cloned[i] = cloneRoute(route)
	}
	return cloned
}

func cloneRoute(route Route) Route {
	route.Method = strings.ToUpper(strings.TrimSpace(route.Method))
	route.Path = strings.TrimSpace(route.Path)
	route.OperationID = strings.TrimSpace(route.OperationID)
	route.Features = cloneRouteFeatures(route.Features)
	route.Middleware = append([]Middleware(nil), route.Middleware...)
	return route
}

func cloneRouteFeatures(features map[string]bool) map[string]bool {
	if len(features) == 0 {
		return nil
	}
	cloned := make(map[string]bool, len(features))
	for key, value := range features {
		if strings.TrimSpace(key) == "" {
			continue
		}
		cloned[key] = value
	}
	return cloned
}
