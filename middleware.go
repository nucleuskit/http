package runtimehttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	authcap "github.com/nucleuskit/cap/auth"
	logcap "github.com/nucleuskit/cap/log"
	metriccap "github.com/nucleuskit/cap/metric"
	capsentinel "github.com/nucleuskit/cap/sentinel"
	tracecap "github.com/nucleuskit/cap/trace"
	nucleuscontext "github.com/nucleuskit/core/context"
	coreerrors "github.com/nucleuskit/core/errors"
	"github.com/nucleuskit/core/response"
)

const requestIDHeader = "X-Request-Id"

type Middleware func(http.Handler) http.Handler

type Validator interface {
	Validate(*http.Request) error
}

type ValidatorFunc func(*http.Request) error

func (fn ValidatorFunc) Validate(request *http.Request) error {
	if fn == nil {
		return nil
	}
	return fn(request)
}

type ServerFinalizer interface {
	FinalizeHTTP(context.Context, FinalizerEvent)
}

type FinalizerFunc func(context.Context, FinalizerEvent)

func (fn FinalizerFunc) FinalizeHTTP(ctx context.Context, event FinalizerEvent) {
	if fn != nil {
		fn(ctx, event)
	}
}

type FinalizerEvent struct {
	Method     string
	Path       string
	Operation  string
	StatusCode int
	Duration   time.Duration
	TraceID    string
	RequestID  string
}

type principalKey struct{}

var requestIDCounter uint64

type DefaultMiddlewareOption func(*DefaultMiddlewareOptions)

type DefaultMiddlewareOptions struct {
	Logger        logcap.Logger
	Tracer        tracecap.Tracer
	Meter         metriccap.Meter
	Breaker       capsentinel.Breaker
	Limiter       capsentinel.Limiter
	Authenticator authcap.Authenticator
	Authorizer    authcap.Authorizer
	AuthOptions   []AuthOption
	Timeout       time.Duration
	MaxBytes      int64
	MaxConns      int
	Gunzip        bool
	Validators    []Validator
	Finalizers    []ServerFinalizer
	Middleware    []Middleware
}

type AuthOption func(*authOptions)

type authOptions struct {
	credentials func(*http.Request) authcap.Credentials
	permission  func(*http.Request, authcap.Principal) authcap.Permission
}

func Recovery() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					err := coreerrors.New(coreerrors.CodeInternal, coreerrors.CodeInternal.DefaultMessage())
					writeJSON(writer, http.StatusInternalServerError, response.Error(err, traceID(request)))
				}
			}()
			next.ServeHTTP(writer, request)
		})
	}
}

func AccessLog(logger logcap.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		if logger == nil {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			capture := newStatusRecorder(writer)
			startedAt := time.Now()
			next.ServeHTTP(capture, request)

			status := capture.statusCode()
			fields := []logcap.Field{
				logcap.String("method", request.Method),
				logcap.String("path", request.URL.Path),
				logcap.Any("status", status),
				logcap.Any("duration_ms", float64(time.Since(startedAt).Microseconds())/1000),
			}
			if status >= http.StatusInternalServerError {
				logger.Error(request.Context(), "http request completed", fields...)
				return
			}
			logger.Info(request.Context(), "http request completed", fields...)
		})
	}
}

func Trace(tracer tracecap.Tracer) Middleware {
	return func(next http.Handler) http.Handler {
		if tracer == nil {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			ctx, span := tracer.Start(request.Context(), "http.server",
				tracecap.String("http.method", request.Method),
				tracecap.String("http.path", request.URL.Path),
			)
			if span == nil {
				next.ServeHTTP(writer, request.WithContext(ctx))
				return
			}
			capture := newStatusRecorder(writer)
			defer span.End()

			next.ServeHTTP(capture, request.WithContext(ctx))
			status := capture.statusCode()
			span.SetAttribute("http.status_code", status)
			if status >= http.StatusInternalServerError {
				span.RecordError(errors.New(http.StatusText(status)))
			}
		})
	}
}

func Metric(meter metriccap.Meter) Middleware {
	return func(next http.Handler) http.Handler {
		if meter == nil {
			return next
		}
		requests := meter.Counter("http.server.requests")
		duration := meter.Gauge("http.server.duration_ms")
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			capture := newStatusRecorder(writer)
			startedAt := time.Now()
			next.ServeHTTP(capture, request)

			attrs := []metriccap.Attribute{
				metriccap.String("method", request.Method),
				metriccap.String("path", request.URL.Path),
				metriccap.Any("status", capture.statusCode()),
			}
			if requests != nil {
				requests.Add(request.Context(), 1, attrs...)
			}
			if duration != nil {
				duration.Record(request.Context(), float64(time.Since(startedAt).Microseconds())/1000, attrs...)
			}
		})
	}
}

func Sentinel(breaker capsentinel.Breaker, limiter capsentinel.Limiter) Middleware {
	return func(next http.Handler) http.Handler {
		if breaker == nil && limiter == nil {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			resource := sentinelResource(request)
			guard, permit, err := acquireSentinel(request.Context(), resource, breaker, limiter)
			if err != nil {
				writeSentinelError(writer, request)
				return
			}
			if permit != nil {
				defer permit.Release()
			}
			if guard == nil {
				next.ServeHTTP(writer, request)
				return
			}

			capture := newStatusRecorder(writer)
			var doneErr error
			defer func() {
				if recovered := recover(); recovered != nil {
					guard.Done(fmt.Errorf("panic: %v", recovered))
					panic(recovered)
				}
				guard.Done(doneErr)
			}()
			next.ServeHTTP(capture, request)
			if status := capture.statusCode(); status >= http.StatusInternalServerError {
				doneErr = errors.New(http.StatusText(status))
			}
		})
	}
}

func Auth(authenticator authcap.Authenticator, authorizer authcap.Authorizer) Middleware {
	return AuthWithOptions(authenticator, authorizer)
}

func AuthWithOptions(authenticator authcap.Authenticator, authorizer authcap.Authorizer, options ...AuthOption) Middleware {
	config := newAuthOptions(options...)
	return func(next http.Handler) http.Handler {
		if authenticator == nil {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			principal, err := authenticator.Authenticate(request.Context(), config.credentials(request))
			if err != nil {
				writeAuthError(writer, request, coreerrors.CodeUnauthenticated)
				return
			}
			if authorizer != nil {
				permission := config.permission(request, principal)
				if err := authorizer.Authorize(request.Context(), principal, permission); err != nil {
					writeAuthError(writer, request, coreerrors.CodePermissionDenied)
					return
				}
			}
			ctx := contextWithPrincipal(request.Context(), principal)
			if principal.Tenant != "" {
				ctx = nucleuscontext.WithTenant(ctx, principal.Tenant)
			}
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	}
}

func WithAuthCredentials(fn func(*http.Request) authcap.Credentials) AuthOption {
	return func(options *authOptions) {
		if fn != nil {
			options.credentials = fn
		}
	}
}

func WithAuthPermission(fn func(*http.Request, authcap.Principal) authcap.Permission) AuthOption {
	return func(options *authOptions) {
		if fn != nil {
			options.permission = fn
		}
	}
}

func WithDefaultLogger(logger logcap.Logger) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Logger = logger
	}
}

func WithDefaultTracer(tracer tracecap.Tracer) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Tracer = tracer
	}
}

func WithDefaultMeter(meter metriccap.Meter) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Meter = meter
	}
}

func WithDefaultSentinel(breaker capsentinel.Breaker, limiter capsentinel.Limiter) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Breaker = breaker
		options.Limiter = limiter
	}
}

func WithDefaultAuth(authenticator authcap.Authenticator, authorizer authcap.Authorizer, authOptions ...AuthOption) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Authenticator = authenticator
		options.Authorizer = authorizer
		options.AuthOptions = append([]AuthOption(nil), authOptions...)
	}
}

func WithDefaultTimeout(timeout time.Duration) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Timeout = timeout
	}
}

func WithDefaultMaxBytes(limit int64) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.MaxBytes = limit
	}
}

func WithDefaultMaxConns(limit int) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.MaxConns = limit
	}
}

func WithDefaultGunzip(enabled bool) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Gunzip = enabled
	}
}

func WithDefaultValidation(validators ...Validator) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Validators = append(options.Validators, validators...)
	}
}

func WithDefaultFinalizers(finalizers ...ServerFinalizer) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Finalizers = append(options.Finalizers, finalizers...)
	}
}

func WithDefaultMiddleware(middleware ...Middleware) DefaultMiddlewareOption {
	return func(options *DefaultMiddlewareOptions) {
		options.Middleware = append(options.Middleware, middleware...)
	}
}

func DefaultMiddlewareChain(options ...DefaultMiddlewareOption) []Middleware {
	config := NewDefaultMiddlewareOptions(options...)
	middleware := []Middleware{
		RequestID(),
		Recovery(),
	}
	if config.MaxConns > 0 {
		middleware = append(middleware, MaxConns(config.MaxConns))
	}
	if config.Gunzip {
		middleware = append(middleware, Gunzip())
	}
	if config.MaxBytes > 0 {
		middleware = append(middleware, MaxBytes(config.MaxBytes))
	}
	if config.Timeout > 0 {
		middleware = append(middleware, Timeout(config.Timeout))
	}
	if len(config.Finalizers) > 0 {
		middleware = append(middleware, Finalize(config.Finalizers...))
	}
	if config.Tracer != nil {
		middleware = append(middleware, Trace(config.Tracer))
	}
	if config.Logger != nil {
		middleware = append(middleware, AccessLog(config.Logger))
	}
	if config.Meter != nil {
		middleware = append(middleware, Metric(config.Meter))
	}
	if config.Breaker != nil || config.Limiter != nil {
		middleware = append(middleware, Sentinel(config.Breaker, config.Limiter))
	}
	if config.Authenticator != nil {
		middleware = append(middleware, AuthWithOptions(config.Authenticator, config.Authorizer, config.AuthOptions...))
	}
	if len(config.Validators) > 0 {
		middleware = append(middleware, Validation(config.Validators...))
	}
	middleware = append(middleware, config.Middleware...)
	return middleware
}

func NewDefaultMiddlewareOptions(options ...DefaultMiddlewareOption) DefaultMiddlewareOptions {
	config := DefaultMiddlewareOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	config.AuthOptions = append([]AuthOption(nil), config.AuthOptions...)
	config.Validators = append([]Validator(nil), config.Validators...)
	config.Finalizers = append([]ServerFinalizer(nil), config.Finalizers...)
	config.Middleware = append([]Middleware(nil), config.Middleware...)
	return config
}

func PrincipalFromContext(ctx context.Context) (authcap.Principal, bool) {
	if principal, ok := authcap.PrincipalFromContext(ctx); ok {
		return principal, true
	}
	principal, ok := ctx.Value(principalKey{}).(authcap.Principal)
	return principal, ok
}

func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requestID := request.Header.Get(requestIDHeader)
			if requestID == "" {
				requestID = nextRequestID()
			}
			writer.Header().Set(requestIDHeader, requestID)
			request = request.WithContext(nucleuscontext.WithRequestID(request.Context(), requestID))
			next.ServeHTTP(writer, request)
		})
	}
}

func Timeout(timeout time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		if timeout <= 0 {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			ctx, cancel := context.WithTimeout(request.Context(), timeout)
			defer cancel()
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	}
}

func Validation(validators ...Validator) Middleware {
	active := make([]Validator, 0, len(validators))
	for _, validator := range validators {
		if validator != nil {
			active = append(active, validator)
		}
	}
	return func(next http.Handler) http.Handler {
		if len(active) == 0 {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			for _, validator := range active {
				if err := validator.Validate(request); err != nil {
					err = validationError(err)
					writeJSON(writer, statusFromError(err), response.Error(err, traceID(request)))
					return
				}
			}
			next.ServeHTTP(writer, request)
		})
	}
}

func Finalize(finalizers ...ServerFinalizer) Middleware {
	active := make([]ServerFinalizer, 0, len(finalizers))
	for _, finalizer := range finalizers {
		if finalizer != nil {
			active = append(active, finalizer)
		}
	}
	return func(next http.Handler) http.Handler {
		if len(active) == 0 {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			capture := newStatusRecorder(writer)
			startedAt := time.Now()
			defer func() {
				if recovered := recover(); recovered != nil {
					runFinalizers(request, active, finalizerStatus(capture, true), time.Since(startedAt))
					panic(recovered)
				}
				runFinalizers(request, active, finalizerStatus(capture, false), time.Since(startedAt))
			}()
			next.ServeHTTP(capture, request)
		})
	}
}

func Chain(handler http.Handler, middleware ...Middleware) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		if middleware[i] == nil {
			continue
		}
		handler = middleware[i](handler)
	}
	return handler
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func newStatusRecorder(writer http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: writer}
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(body)
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func contextWithPrincipal(ctx context.Context, principal authcap.Principal) context.Context {
	ctx = authcap.ContextWithPrincipal(ctx, principal)
	return context.WithValue(ctx, principalKey{}, principal)
}

func validationError(err error) error {
	if err == nil {
		return nil
	}
	var codeErr *coreerrors.CodeError
	if errors.As(err, &codeErr) {
		return err
	}
	return coreerrors.New(coreerrors.CodeInvalidArgument, err.Error())
}

func runFinalizers(request *http.Request, finalizers []ServerFinalizer, status int, duration time.Duration) {
	event := finalizerEvent(request, status, duration)
	for _, finalizer := range finalizers {
		finalizer.FinalizeHTTP(request.Context(), event)
	}
}

func finalizerStatus(capture *statusRecorder, recovered bool) int {
	if recovered && capture.status == 0 {
		return http.StatusInternalServerError
	}
	return capture.statusCode()
}

func finalizerEvent(request *http.Request, status int, duration time.Duration) FinalizerEvent {
	route, _ := routeContext(request.Context())
	method := request.Method
	path := request.URL.Path
	if route.Method != "" {
		method = route.Method
	}
	if route.Path != "" {
		path = route.Path
	}
	requestID := request.Header.Get(requestIDHeader)
	if requestID == "" {
		requestID = nucleuscontext.RequestID(request.Context())
	}
	return FinalizerEvent{
		Method:     method,
		Path:       path,
		Operation:  route.Operation,
		StatusCode: status,
		Duration:   duration,
		TraceID:    traceID(request),
		RequestID:  requestID,
	}
}

func writeAuthError(writer http.ResponseWriter, request *http.Request, code coreerrors.Code) {
	err := coreerrors.New(code, code.DefaultMessage())
	writeJSON(writer, coreerrors.HTTPStatus(code), response.Error(err, traceID(request)))
}

func writeSentinelError(writer http.ResponseWriter, request *http.Request) {
	err := coreerrors.New(coreerrors.CodeUnavailable, coreerrors.CodeUnavailable.DefaultMessage())
	writeJSON(writer, http.StatusTooManyRequests, response.Error(err, traceID(request)))
}

func nextRequestID() string {
	value := atomic.AddUint64(&requestIDCounter, 1)
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), value)
}

func newAuthOptions(options ...AuthOption) authOptions {
	config := authOptions{
		credentials: defaultAuthCredentials,
		permission:  defaultAuthPermission,
	}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	return config
}

func defaultAuthCredentials(request *http.Request) authcap.Credentials {
	return authcap.CredentialsFromHeaders(request.Header)
}

func defaultAuthPermission(request *http.Request, _ authcap.Principal) authcap.Permission {
	return authcap.PermissionFor(request.URL.Path, request.Method)
}

func acquireSentinel(ctx context.Context, resource capsentinel.Resource, breaker capsentinel.Breaker, limiter capsentinel.Limiter) (capsentinel.Guard, capsentinel.Permit, error) {
	var guard capsentinel.Guard
	var permit capsentinel.Permit
	var err error
	if breaker != nil {
		guard, err = breaker.Allow(ctx, resource)
		if err != nil {
			return nil, nil, err
		}
	}
	if limiter != nil {
		permit, err = limiter.Acquire(ctx, resource)
		if err != nil {
			if guard != nil {
				guard.Done(err)
			}
			return nil, nil, err
		}
	}
	return guard, permit, nil
}

func sentinelResource(request *http.Request) capsentinel.Resource {
	resourceName := request.Method + " " + request.URL.Path
	attributes := []capsentinel.Attribute{
		capsentinel.String("component", "http.server"),
		capsentinel.String("method", request.Method),
		capsentinel.String("path", request.URL.Path),
	}
	if routePriority, ok := RoutePriorityFromContext(request.Context()); ok {
		attributes = append(attributes, capsentinel.String("priority", strconv.Itoa(routePriority)))
	}
	return capsentinel.Resource{
		Name:       resourceName,
		Attributes: attributes,
	}
}
