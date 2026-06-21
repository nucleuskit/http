package runtimehttp

import (
	"context"
	"net/http"
	"strings"
)

type RouteInfo struct {
	Method      string
	Path        string
	OperationID string
	Features    map[string]bool
}

type RouteMatcher func(RouteInfo) bool

func SelectRoute(middleware Middleware, matchers ...RouteMatcher) Middleware {
	return func(next http.Handler) http.Handler {
		if middleware == nil {
			return next
		}
		selected := middleware(next)
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if routeMatches(routeInfo(request), matchers...) {
				selected.ServeHTTP(writer, request)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
}

func Feature(name string, middleware Middleware) Middleware {
	return SelectRoute(middleware, MatchFeature(name))
}

func MatchOperationID(operationIDs ...string) RouteMatcher {
	allowed := stringSet(operationIDs...)
	return func(route RouteInfo) bool {
		return allowed[strings.TrimSpace(route.OperationID)]
	}
}

func MatchRoute(method string, path string) RouteMatcher {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	return func(route RouteInfo) bool {
		return strings.EqualFold(route.Method, method) && route.Path == path
	}
}

func MatchPath(paths ...string) RouteMatcher {
	allowed := stringSet(paths...)
	return func(route RouteInfo) bool {
		return allowed[route.Path]
	}
}

func MatchFeature(name string) RouteMatcher {
	name = strings.TrimSpace(name)
	return func(route RouteInfo) bool {
		return name != "" && route.Features[name]
	}
}

func RouteInfoFromContext(ctx context.Context) (RouteInfo, bool) {
	route, ok := routeContext(ctx)
	if !ok {
		return RouteInfo{}, false
	}
	return RouteInfo{
		Method:      route.Method,
		Path:        route.Path,
		OperationID: route.Operation,
		Features:    cloneRouteFeatures(route.Features),
	}, true
}

func FeatureEnabled(ctx context.Context, name string) bool {
	route, ok := RouteInfoFromContext(ctx)
	return ok && route.Features[strings.TrimSpace(name)]
}

func routeInfo(request *http.Request) RouteInfo {
	if route, ok := RouteInfoFromContext(request.Context()); ok {
		if route.Method == "" {
			route.Method = request.Method
		}
		if route.Path == "" {
			route.Path = request.URL.Path
		}
		return route
	}
	return RouteInfo{
		Method: strings.ToUpper(strings.TrimSpace(request.Method)),
		Path:   request.URL.Path,
	}
}

func routeMatches(route RouteInfo, matchers ...RouteMatcher) bool {
	if len(matchers) == 0 {
		return true
	}
	for _, matcher := range matchers {
		if matcher != nil && matcher(route) {
			return true
		}
	}
	return false
}

func stringSet(values ...string) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result[value] = true
		}
	}
	return result
}
