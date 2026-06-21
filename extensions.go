package runtimehttp

import (
	"fmt"
	"html"
	"net/http"
	"net/http/pprof"
	"strings"
)

const (
	defaultOpenAPISpecPath = "/openapi.yaml"
	defaultSwaggerPath     = "/swagger/"
)

type OpenAPIOptions struct {
	Spec        []byte
	SpecPath    string
	SwaggerPath string
	Title       string
	ContentType string
}

func WithPProf() Option {
	return func(server *Server) {
		server.mux.Handle("GET /debug/pprof/", http.HandlerFunc(pprof.Index))
		server.mux.Handle("GET /debug/pprof/{profile}", http.HandlerFunc(pprof.Index))
		server.mux.Handle("GET /debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
		server.mux.Handle("GET /debug/pprof/profile", http.HandlerFunc(pprof.Profile))
		server.mux.Handle("GET /debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
		server.mux.Handle("GET /debug/pprof/trace", http.HandlerFunc(pprof.Trace))
	}
}

func WithOpenAPI(options OpenAPIOptions) Option {
	return func(server *Server) {
		if len(options.Spec) == 0 {
			return
		}
		specPath := cleanPath(options.SpecPath, defaultOpenAPISpecPath)
		swaggerPath := cleanPath(options.SwaggerPath, defaultSwaggerPath)
		contentType := options.ContentType
		if contentType == "" {
			contentType = "application/yaml; charset=utf-8"
		}
		title := options.Title
		if title == "" {
			title = "Nucleus OpenAPI"
		}

		spec := append([]byte(nil), options.Spec...)
		server.mux.Handle("GET "+specPath, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", contentType)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(spec)
		}))
		server.mux.Handle("GET "+swaggerPath, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(swaggerHTML(title, specPath)))
		}))
	}
}

func cleanPath(path string, fallback string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return fallback
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func swaggerHTML(title string, specPath string) string {
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>%s</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({url: %q, dom_id: "#swagger-ui"});
  </script>
</body>
</html>
`, html.EscapeString(title), specPath)
}
