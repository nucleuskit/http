package runtimehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	coreerrors "github.com/nucleuskit/nucleus/core/errors"
	"github.com/nucleuskit/nucleus/core/response"
)

type Server struct {
	mux             *http.ServeMux
	middleware      []Middleware
	shutdownTimeout time.Duration
	mu              sync.Mutex
	active          *http.Server
	routes          []Route
}

type Handler func(*http.Request) (any, error)

type ResponseEncoder interface {
	EncodeHTTPResponse(http.ResponseWriter, *http.Request, any, error)
}

type ResponseEncoderFunc func(http.ResponseWriter, *http.Request, any, error)

type Option func(*Server)

type StatusResponse struct {
	StatusCode int
	Data       any
}

func WithStatus(status int, data any) StatusResponse {
	return StatusResponse{StatusCode: status, Data: data}
}

func NewServer(options ...Option) *Server {
	server := &Server{
		mux:             http.NewServeMux(),
		shutdownTimeout: 30 * time.Second,
	}
	for _, option := range options {
		option(server)
	}
	return server
}

func WithMiddleware(middleware ...Middleware) Option {
	return func(server *Server) {
		server.Use(middleware...)
	}
}

func WithShutdownTimeout(timeout time.Duration) Option {
	return func(server *Server) {
		if timeout < 0 {
			timeout = 0
		}
		server.shutdownTimeout = timeout
	}
}

func (s *Server) Use(middleware ...Middleware) {
	s.middleware = append(s.middleware, middleware...)
}

func (s *Server) Handle(method string, path string, handler Handler) {
	s.HandleRoute(Route{Method: method, Path: path, Handler: handler})
}

func ResponseEnvelope(handler Handler) http.Handler {
	return ResponseEnvelopeWithEncoder(handler, nil)
}

func ResponseEnvelopeWithEncoder(handler Handler, encoder ResponseEncoder) http.Handler {
	if encoder == nil {
		encoder = ResponseEncoderFunc(defaultEncodeHTTPResponse)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, err := handler(request)
		encoder.EncodeHTTPResponse(writer, request, data, err)
	})
}

func (fn ResponseEncoderFunc) EncodeHTTPResponse(writer http.ResponseWriter, request *http.Request, data any, err error) {
	if fn != nil {
		fn(writer, request, data, err)
	}
}

func (s *Server) Handler() http.Handler {
	var handler http.Handler = s.mux
	for i := len(s.middleware) - 1; i >= 0; i-- {
		handler = s.middleware[i](handler)
	}
	return s.routeContextHandler(handler)
}

func (s *Server) ListenAndServe(addr string) error {
	return s.Run(context.Background(), addr)
}

func (s *Server) Run(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}

func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil {
		ctx = context.Background()
	}
	httpServer := &http.Server{Handler: s.Handler()}
	s.setActive(httpServer)
	defer s.clearActive(httpServer)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := serverShutdownContext(s.shutdownTimeout)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		case <-done:
		}
	}()
	defer close(done)

	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) Routes() []Route {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRoutes(s.routes)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	httpServer := s.active
	s.mu.Unlock()
	if httpServer == nil {
		return nil
	}
	return httpServer.Shutdown(ctx)
}

func (s *Server) setActive(httpServer *http.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = httpServer
}

func (s *Server) clearActive(httpServer *http.Server) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == httpServer {
		s.active = nil
	}
}

func writeJSON(writer http.ResponseWriter, status int, envelope response.Envelope) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(envelope)
}

func defaultEncodeHTTPResponse(writer http.ResponseWriter, request *http.Request, data any, err error) {
	if err != nil {
		writeJSON(writer, statusFromError(err), response.Error(err, traceID(request)))
		return
	}
	status := http.StatusOK
	if payload, ok := data.(StatusResponse); ok {
		status = payload.StatusCode
		data = payload.Data
	}
	if status < 100 {
		status = http.StatusOK
	}
	writeJSON(writer, status, response.OK(data, traceID(request)))
}

func traceID(request *http.Request) string {
	if traceID := request.Header.Get("traceparent"); traceID != "" {
		return traceID
	}
	return request.Header.Get("X-Request-Id")
}

func routePattern(method string, path string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return path
	}
	return method + " " + path
}

func serverShutdownContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func statusFromError(err error) int {
	var codeErr *coreerrors.CodeError
	if errors.As(err, &codeErr) {
		return coreerrors.HTTPStatus(codeErr.Code)
	}
	return http.StatusInternalServerError
}
