// Package http provides thin HTTP runtime primitives for generated Nucleus adapters.
package http

import (
	"encoding/json"
	"fmt"
	stdhttp "net/http"
	"strings"
	"sync"
)

// Handler handles one generated route.
type Handler func(request *stdhttp.Request) (any, error)

// Route describes one HTTP route registered with a Server.
type Route struct {
	Method      string
	Path        string
	OperationID string
	Handler     Handler
}

// Server dispatches registered Nucleus HTTP routes.
type Server struct {
	mu     sync.RWMutex
	routes []Route
}

// NewServer creates an empty HTTP route server.
func NewServer() *Server {
	return &Server{}
}

// RegisterRoutes appends generated routes to the server.
func (server *Server) RegisterRoutes(routes []Route) {
	if server == nil {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, route := range routes {
		route.Method = strings.ToUpper(strings.TrimSpace(route.Method))
		server.routes = append(server.routes, route)
	}
}

// Handle registers a single route handler.
func (server *Server) Handle(method string, path string, handler Handler) {
	server.RegisterRoutes([]Route{{
		Method:  method,
		Path:    path,
		Handler: handler,
	}})
}

// ServeHTTP dispatches a request to the first matching registered route.
func (server *Server) ServeHTTP(writer stdhttp.ResponseWriter, request *stdhttp.Request) {
	if server == nil {
		stdhttp.NotFound(writer, request)
		return
	}
	route, ok := server.match(request.Method, request.URL.Path)
	if !ok || route.Handler == nil {
		stdhttp.NotFound(writer, request)
		return
	}
	value, err := route.Handler(request)
	if err != nil {
		writeError(writer, stdhttp.StatusInternalServerError)
		return
	}
	if err := writeResponse(writer, value); err != nil {
		writeError(writer, stdhttp.StatusInternalServerError)
	}
}

func (server *Server) match(method string, path string) (Route, bool) {
	server.mu.RLock()
	defer server.mu.RUnlock()
	method = strings.ToUpper(strings.TrimSpace(method))
	for _, route := range server.routes {
		if route.Method != "" && route.Method != method {
			continue
		}
		if pathMatches(route.Path, path) {
			return route, true
		}
	}
	return Route{}, false
}

func pathMatches(pattern string, path string) bool {
	if pattern == path {
		return true
	}
	patternParts := pathParts(pattern)
	pathParts := pathParts(path)
	if len(patternParts) != len(pathParts) {
		return false
	}
	for index, patternPart := range patternParts {
		if strings.HasPrefix(patternPart, "{") && strings.HasSuffix(patternPart, "}") {
			continue
		}
		if patternPart != pathParts[index] {
			return false
		}
	}
	return true
}

func pathParts(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func writeResponse(writer stdhttp.ResponseWriter, value any) error {
	switch typed := value.(type) {
	case nil:
		writer.WriteHeader(stdhttp.StatusNoContent)
		return nil
	case []byte:
		_, err := writer.Write(typed)
		return err
	case string:
		_, err := fmt.Fprintln(writer, typed)
		return err
	default:
		writer.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(writer).Encode(value)
	}
}

func writeError(writer stdhttp.ResponseWriter, status int) {
	stdhttp.Error(writer, stdhttp.StatusText(status), status)
}
