package runtimehttp

import (
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

func WithFileServer(prefix string, root string) Option {
	return func(server *Server) {
		server.HandleStatic(prefix, root)
	}
}

func (s *Server) HandleStatic(prefix string, root string) {
	prefix = cleanStaticPrefix(prefix)
	root = filepath.Clean(root)
	fileServer := http.FileServer(http.Dir(root))
	s.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if !strings.HasPrefix(request.URL.Path, prefix) {
				next.ServeHTTP(writer, request)
				return
			}
			if staticPathEscapes(strings.TrimPrefix(request.URL.Path, prefix)) {
				http.NotFound(writer, request)
				return
			}
			http.StripPrefix(prefix, fileServer).ServeHTTP(writer, request)
		})
	})
}

func cleanStaticPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "/"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return prefix
}

func staticPathEscapes(name string) bool {
	if name == "" {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return true
		}
	}
	cleaned := path.Clean("/" + name)
	return strings.HasPrefix(cleaned, "/../") || cleaned == "/.."
}
