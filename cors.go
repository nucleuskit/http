package runtimehttp

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type CORSOptions struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	ExposeHeaders  []string
	Credentials    bool
	MaxAge         time.Duration
}

func CORS(options CORSOptions) Middleware {
	allowedOrigins := stringSet(options.AllowedOrigins...)
	allowedMethods := joinHeaderValues(options.AllowedMethods)
	allowedHeaders := joinHeaderValues(options.AllowedHeaders)
	exposeHeaders := joinHeaderValues(options.ExposeHeaders)
	maxAge := ""
	if options.MaxAge > 0 {
		maxAge = strconv.FormatInt(int64(options.MaxAge/time.Second), 10)
	}
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			origin := strings.TrimSpace(request.Header.Get("Origin"))
			if origin == "" || !corsOriginAllowed(origin, allowedOrigins) {
				if isCORSPreflight(request) {
					http.Error(writer, http.StatusText(http.StatusForbidden), http.StatusForbidden)
					return
				}
				next.ServeHTTP(writer, request)
				return
			}
			writeCORSHeaders(writer.Header(), origin, allowedOrigins, allowedMethods, allowedHeaders, exposeHeaders, maxAge, options.Credentials)
			if isCORSPreflight(request) {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
}

func corsOriginAllowed(origin string, allowed map[string]bool) bool {
	if len(allowed) == 0 {
		return false
	}
	return allowed["*"] || allowed[origin]
}

func writeCORSHeaders(header http.Header, origin string, allowedOrigins map[string]bool, methods string, allowedHeaders string, exposeHeaders string, maxAge string, credentials bool) {
	if allowedOrigins["*"] && !credentials {
		header.Set("Access-Control-Allow-Origin", "*")
	} else {
		header.Set("Access-Control-Allow-Origin", origin)
	}
	addVaryHeader(header, "Origin")
	if methods != "" {
		header.Set("Access-Control-Allow-Methods", methods)
	}
	if allowedHeaders != "" {
		header.Set("Access-Control-Allow-Headers", allowedHeaders)
	}
	if exposeHeaders != "" {
		header.Set("Access-Control-Expose-Headers", exposeHeaders)
	}
	if credentials {
		header.Set("Access-Control-Allow-Credentials", "true")
	}
	if maxAge != "" {
		header.Set("Access-Control-Max-Age", maxAge)
	}
}

func isCORSPreflight(request *http.Request) bool {
	return request.Method == http.MethodOptions && request.Header.Get("Access-Control-Request-Method") != ""
}

func joinHeaderValues(values []string) string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	return strings.Join(cleaned, ", ")
}

func addVaryHeader(header http.Header, value string) {
	for _, existing := range header.Values("Vary") {
		for _, part := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return
			}
		}
	}
	header.Add("Vary", value)
}
