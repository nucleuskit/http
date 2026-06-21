package runtimehttp

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestMiddlewareNilProvidersNoop(t *testing.T) {
	called := false
	handler := chainMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
		writer.WriteHeader(http.StatusAccepted)
	}), AccessLog(nil), Trace(nil), Metric(nil), Sentinel(nil, nil), Auth(nil, nil))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/noop", nil))

	if !called {
		t.Fatal("expected next handler to be called")
	}
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", recorder.Code)
	}
}

func TestDefaultMiddlewareChainNilProvidersNoop(t *testing.T) {
	called := false
	handler := chainMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
		writer.WriteHeader(http.StatusAccepted)
	}), DefaultMiddlewareChain()...)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/noop", nil))

	if !called {
		t.Fatal("expected next handler to be called")
	}
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", recorder.Code)
	}
	if recorder.Header().Get(requestIDHeader) == "" {
		t.Fatal("expected request id middleware to run")
	}
}

func TestAuthMiddlewareRejectsUnauthenticatedWithEnvelope(t *testing.T) {
	handler := Auth(rejectingAuthenticator{err: errors.New("token backend leaked detail")}, nil)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("next handler should not be called")
		}),
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/secure", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != coreerrors.CodeUnauthenticated.Int() {
		t.Fatalf("expected unauthenticated code, got %d", envelope.Code)
	}
	if envelope.Message != coreerrors.CodeUnauthenticated.DefaultMessage() {
		t.Fatalf("expected sanitized message, got %q", envelope.Message)
	}
	if strings.Contains(recorder.Body.String(), "backend leaked") {
		t.Fatalf("auth response leaked internal error: %s", recorder.Body.String())
	}
}

func TestAuthMiddlewareRejectsForbiddenWithEnvelope(t *testing.T) {
	handler := Auth(allowingAuthenticator{}, rejectingAuthorizer{err: errors.New("policy backend detail")})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("next handler should not be called")
		}),
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/secure", nil))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != coreerrors.CodePermissionDenied.Int() {
		t.Fatalf("expected permission denied code, got %d", envelope.Code)
	}
	if envelope.Message != coreerrors.CodePermissionDenied.DefaultMessage() {
		t.Fatalf("expected sanitized message, got %q", envelope.Message)
	}
}

func TestAccessLogMetricTraceMiddlewareAreCalled(t *testing.T) {
	logger := &recordingLogger{}
	meter := newRecordingMeter()
	tracer := &recordingTracer{}
	handler := chainMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	}), AccessLog(logger), Trace(tracer), Metric(meter))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/widgets", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}
	if logger.infoCalls != 1 {
		t.Fatalf("expected one access log info call, got %d", logger.infoCalls)
	}
	if meter.counters["http.server.requests"].adds != 1 {
		t.Fatalf("expected request counter add, got %d", meter.counters["http.server.requests"].adds)
	}
	if meter.gauges["http.server.duration_ms"].records != 1 {
		t.Fatalf("expected duration record, got %d", meter.gauges["http.server.duration_ms"].records)
	}
	if tracer.starts != 1 {
		t.Fatalf("expected one trace start, got %d", tracer.starts)
	}
	if tracer.span.ends != 1 {
		t.Fatalf("expected one trace end, got %d", tracer.span.ends)
	}
	if got := tracer.span.attributes["http.status_code"]; got != http.StatusCreated {
		t.Fatalf("expected span status attribute 201, got %#v", got)
	}
}

func TestSentinelMiddlewareRejectsWithEnvelope(t *testing.T) {
	breaker := &rejectingServerSentinel{err: errors.New("provider leaked detail")}
	handler := Sentinel(breaker, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/orders/1", nil)
	request.Header.Set(requestIDHeader, "req-sentinel")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status 429, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != coreerrors.CodeUnavailable.Int() {
		t.Fatalf("expected unavailable code, got %d", envelope.Code)
	}
	if envelope.Message != coreerrors.CodeUnavailable.DefaultMessage() {
		t.Fatalf("expected sanitized message, got %q", envelope.Message)
	}
	if envelope.TraceID != "req-sentinel" {
		t.Fatalf("expected trace id from request id, got %q", envelope.TraceID)
	}
	if strings.Contains(recorder.Body.String(), "provider leaked") {
		t.Fatalf("sentinel response leaked provider detail: %s", recorder.Body.String())
	}
	if breaker.resource.Name != "DELETE /orders/1" {
		t.Fatalf("unexpected sentinel resource name: %q", breaker.resource.Name)
	}
	if got := sentinelAttribute(breaker.resource, "method"); got != http.MethodDelete {
		t.Fatalf("expected method attribute DELETE, got %#v", got)
	}
	if got := sentinelAttribute(breaker.resource, "path"); got != "/orders/1" {
		t.Fatalf("expected path attribute /orders/1, got %#v", got)
	}
}

func TestDefaultMiddlewareChainCallsConfiguredProvidersInStableOrder(t *testing.T) {
	order := []string{}
	logger := &recordingLogger{order: &order}
	meter := newRecordingMeter()
	meter.order = &order
	tracer := &recordingTracer{order: &order}
	sentinel := &recordingSentinel{order: &order}
	authenticator := &capturingAuthenticator{principal: authcap.Principal{Subject: "alice", Tenant: "tenant-a"}, order: &order}
	authorizer := &capturingAuthorizer{order: &order}

	handler := chainMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		order = append(order, "handler")
		if nucleuscontext.RequestID(request.Context()) == "" {
			t.Fatal("expected request id in context")
		}
		writer.WriteHeader(http.StatusCreated)
	}), DefaultMiddlewareChain(
		WithDefaultLogger(logger),
		WithDefaultTracer(tracer),
		WithDefaultMeter(meter),
		WithDefaultSentinel(sentinel, sentinel),
		WithDefaultAuth(authenticator, authorizer),
	)...)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/widgets", nil))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", recorder.Code)
	}
	want := []string{
		"trace:start",
		"sentinel:allow",
		"sentinel:acquire",
		"auth:authenticate",
		"auth:authorize",
		"handler",
		"sentinel:done",
		"sentinel:release",
		"metric:requests",
		"metric:duration",
		"accesslog:info",
		"trace:end",
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected middleware order:\n got: %v\nwant: %v", order, want)
	}
	if sentinel.guard.doneErr != nil {
		t.Fatalf("expected successful guard done, got %v", sentinel.guard.doneErr)
	}
	if sentinel.permit.releases != 1 {
		t.Fatalf("expected one sentinel release, got %d", sentinel.permit.releases)
	}
}

func TestChainComposesMiddlewareInOrder(t *testing.T) {
	order := []string{}
	handler := Chain(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		order = append(order, "handler")
		writer.WriteHeader(http.StatusAccepted)
	}),
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				order = append(order, "outer:before")
				next.ServeHTTP(writer, request)
				order = append(order, "outer:after")
			})
		},
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				order = append(order, "inner:before")
				next.ServeHTTP(writer, request)
				order = append(order, "inner:after")
			})
		},
	)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/chain", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", recorder.Code)
	}
	want := []string{"outer:before", "inner:before", "handler", "inner:after", "outer:after"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected order:\n got: %v\nwant: %v", order, want)
	}
}

func TestDefaultMiddlewareChainAddsConfiguredP0Extensions(t *testing.T) {
	order := []string{}
	var finalizerEvent FinalizerEvent
	handler := Chain(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		order = append(order, "handler")
		if _, ok := request.Context().Deadline(); !ok {
			t.Fatal("expected timeout middleware to set a deadline")
		}
		writer.WriteHeader(http.StatusAccepted)
	}), DefaultMiddlewareChain(
		WithDefaultTimeout(time.Second),
		WithDefaultValidation(ValidatorFunc(func(*http.Request) error {
			order = append(order, "validation")
			return nil
		})),
		WithDefaultMiddleware(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				order = append(order, "custom:before")
				next.ServeHTTP(writer, request)
				order = append(order, "custom:after")
			})
		}),
		WithDefaultFinalizers(FinalizerFunc(func(ctx context.Context, event FinalizerEvent) {
			order = append(order, "finalizer")
			finalizerEvent = event
		})),
	)...)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/widgets", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", recorder.Code)
	}
	want := []string{"validation", "custom:before", "handler", "custom:after", "finalizer"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected order:\n got: %v\nwant: %v", order, want)
	}
	if finalizerEvent.Method != http.MethodPatch || finalizerEvent.Path != "/widgets" || finalizerEvent.StatusCode != http.StatusAccepted {
		t.Fatalf("unexpected finalizer event: %#v", finalizerEvent)
	}
	if finalizerEvent.Duration <= 0 {
		t.Fatalf("expected finalizer duration, got %#v", finalizerEvent)
	}
}

func TestValidationMiddlewareRejectsWithEnvelope(t *testing.T) {
	handler := Validation(ValidatorFunc(func(*http.Request) error {
		return errors.New("missing required field")
	}))(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	}))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/validate", nil)
	request.Header.Set(requestIDHeader, "req-validation")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", recorder.Code)
	}
	var envelope response.Envelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != coreerrors.CodeInvalidArgument.Int() || envelope.Message != "missing required field" {
		t.Fatalf("unexpected validation envelope: %#v", envelope)
	}
	if envelope.TraceID != "req-validation" {
		t.Fatalf("expected trace id from request id, got %q", envelope.TraceID)
	}
}

func TestTraceMiddlewareRecordsServerError(t *testing.T) {
	tracer := &recordingTracer{}
	handler := Trace(tracer)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/fail", nil))

	if tracer.span.errors != 1 {
		t.Fatalf("expected span error, got %d", tracer.span.errors)
	}
}

func TestAuthMiddlewareStoresPrincipalAndTenant(t *testing.T) {
	handler := Auth(allowingAuthenticator{}, nil)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFromContext(request.Context())
		if !ok {
			t.Fatal("expected principal in context")
		}
		if principal.Subject != "alice" {
			t.Fatalf("expected subject alice, got %q", principal.Subject)
		}
		if tenant := nucleuscontext.Tenant(request.Context()); tenant != "tenant-a" {
			t.Fatalf("expected tenant tenant-a, got %q", tenant)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/secure", nil)
	request.Header.Set("Authorization", "Bearer token")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected status 204, got %d", recorder.Code)
	}
}

func TestAuthMiddlewareSupportsCredentialAndPermissionHooks(t *testing.T) {
	authenticator := &capturingAuthenticator{principal: authcap.Principal{Subject: "bob", Tenant: "tenant-b"}}
	authorizer := &capturingAuthorizer{}
	handler := AuthWithOptions(authenticator, authorizer,
		WithAuthCredentials(func(*http.Request) authcap.Credentials {
			return authcap.Credentials{Scheme: "Token", Token: "custom-token", Claims: map[string]any{"sub": "bob"}}
		}),
		WithAuthPermission(func(request *http.Request, principal authcap.Principal) authcap.Permission {
			return authcap.Permission{Resource: "orders", Action: "read", Scope: principal.Tenant}
		}),
	)(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if principal, ok := authcap.PrincipalFromContext(request.Context()); !ok || principal.Subject != "bob" {
			t.Fatalf("expected cap auth principal in context, got %#v %v", principal, ok)
		}
		writer.WriteHeader(http.StatusAccepted)
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/secure", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("expected status 202, got %d", recorder.Code)
	}
	if authenticator.credentials.Token != "custom-token" || authenticator.credentials.Scheme != "Token" {
		t.Fatalf("unexpected credentials: %#v", authenticator.credentials)
	}
	if authorizer.permission.Resource != "orders" || authorizer.permission.Action != "read" || authorizer.permission.Scope != "tenant-b" {
		t.Fatalf("unexpected permission: %#v", authorizer.permission)
	}
}

func TestRuntimeHTTPDoesNotImportBridge(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(".", entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			importSpec, ok := node.(*ast.ImportSpec)
			if !ok {
				return true
			}
			importPath := strings.Trim(importSpec.Path.Value, `"`)
			if strings.Contains(importPath, "/bridge/") {
				t.Fatalf("runtime/http must not import bridge, found %s in %s", importPath, path)
			}
			return true
		})
	}
}

func chainMiddleware(handler http.Handler, middleware ...Middleware) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	return handler
}

type rejectingAuthenticator struct {
	err error
}

func (a rejectingAuthenticator) Authenticate(context.Context, authcap.Credentials) (authcap.Principal, error) {
	return authcap.Principal{}, a.err
}

type allowingAuthenticator struct{}

func (allowingAuthenticator) Authenticate(context.Context, authcap.Credentials) (authcap.Principal, error) {
	return authcap.Principal{Subject: "alice", Tenant: "tenant-a"}, nil
}

type rejectingAuthorizer struct {
	err error
}

func (a rejectingAuthorizer) Authorize(context.Context, authcap.Principal, authcap.Permission) error {
	return a.err
}

type capturingAuthenticator struct {
	principal   authcap.Principal
	credentials authcap.Credentials
	order       *[]string
}

func (a *capturingAuthenticator) Authenticate(_ context.Context, credentials authcap.Credentials) (authcap.Principal, error) {
	appendOrder(a.order, "auth:authenticate")
	a.credentials = credentials
	return a.principal, nil
}

type capturingAuthorizer struct {
	principal  authcap.Principal
	permission authcap.Permission
	order      *[]string
}

func (a *capturingAuthorizer) Authorize(_ context.Context, principal authcap.Principal, permission authcap.Permission) error {
	appendOrder(a.order, "auth:authorize")
	a.principal = principal
	a.permission = permission
	return nil
}

type recordingLogger struct {
	infoCalls  int
	errorCalls int
	order      *[]string
}

func (l *recordingLogger) Debug(context.Context, string, ...logcap.Field) {}
func (l *recordingLogger) Warn(context.Context, string, ...logcap.Field)  {}

func (l *recordingLogger) Info(context.Context, string, ...logcap.Field) {
	appendOrder(l.order, "accesslog:info")
	l.infoCalls++
}

func (l *recordingLogger) Error(context.Context, string, ...logcap.Field) {
	appendOrder(l.order, "accesslog:error")
	l.errorCalls++
}

type recordingTracer struct {
	starts int
	span   *recordingSpan
	order  *[]string
}

func (t *recordingTracer) Start(ctx context.Context, _ string, _ ...tracecap.Attribute) (context.Context, tracecap.Span) {
	appendOrder(t.order, "trace:start")
	t.starts++
	t.span = &recordingSpan{attributes: map[string]any{}, order: t.order}
	return ctx, t.span
}

func (t *recordingTracer) Inject(context.Context, tracecap.Carrier) {}

func (t *recordingTracer) Extract(ctx context.Context, _ tracecap.Carrier) context.Context {
	return ctx
}

type recordingSpan struct {
	attributes map[string]any
	errors     int
	ends       int
	order      *[]string
}

func (s *recordingSpan) Context() tracecap.SpanContext {
	return tracecap.SpanContext{}
}

func (s *recordingSpan) SetAttribute(key string, value any) {
	s.attributes[key] = value
}

func (s *recordingSpan) RecordError(error) {
	s.errors++
}

func (s *recordingSpan) End() {
	appendOrder(s.order, "trace:end")
	s.ends++
}

type recordingMeter struct {
	counters map[string]*recordingCounter
	gauges   map[string]*recordingGauge
	order    *[]string
}

func newRecordingMeter() *recordingMeter {
	return &recordingMeter{
		counters: map[string]*recordingCounter{},
		gauges:   map[string]*recordingGauge{},
	}
}

func (m *recordingMeter) Counter(name string, _ ...metriccap.InstrumentOption) metriccap.Counter {
	counter := &recordingCounter{order: m.order, event: "metric:requests"}
	m.counters[name] = counter
	return counter
}

func (m *recordingMeter) Gauge(name string, _ ...metriccap.InstrumentOption) metriccap.Gauge {
	gauge := &recordingGauge{order: m.order, event: "metric:duration"}
	m.gauges[name] = gauge
	return gauge
}

func (m *recordingMeter) Histogram(name string, _ ...metriccap.InstrumentOption) metriccap.Histogram {
	histogram := &recordingHistogram{}
	m.gauges[name] = &recordingGauge{}
	return histogram
}

func (m *recordingMeter) Snapshot() map[string]float64 {
	return map[string]float64{}
}

type recordingCounter struct {
	adds    int
	records int
	order   *[]string
	event   string
}

func (c *recordingCounter) Descriptor() metriccap.Descriptor {
	return metriccap.Descriptor{}
}

func (c *recordingCounter) Add(context.Context, float64, ...metriccap.Attribute) {
	appendOrder(c.order, c.event)
	c.adds++
}

func (c *recordingCounter) Record(context.Context, float64, ...metriccap.Attribute) {
	c.records++
}

type recordingGauge struct {
	sets    int
	records int
	order   *[]string
	event   string
}

func (g *recordingGauge) Descriptor() metriccap.Descriptor {
	return metriccap.Descriptor{}
}

func (g *recordingGauge) Set(context.Context, float64, ...metriccap.Attribute) {
	g.sets++
}

func (g *recordingGauge) Record(context.Context, float64, ...metriccap.Attribute) {
	appendOrder(g.order, g.event)
	g.records++
}

type recordingHistogram struct {
	observes int
}

func (h *recordingHistogram) Descriptor() metriccap.Descriptor {
	return metriccap.Descriptor{}
}

func (h *recordingHistogram) Observe(context.Context, float64, ...metriccap.Attribute) {
	h.observes++
}

func (h *recordingHistogram) Record(ctx context.Context, value float64, attributes ...metriccap.Attribute) {
	h.Observe(ctx, value, attributes...)
}

type recordingSentinel struct {
	order    *[]string
	resource capsentinel.Resource
	guard    *recordingSentinelGuard
	permit   *recordingSentinelPermit
}

func (s *recordingSentinel) Allow(_ context.Context, resource capsentinel.Resource) (capsentinel.Guard, error) {
	appendOrder(s.order, "sentinel:allow")
	s.resource = resource
	s.guard = &recordingSentinelGuard{order: s.order}
	return s.guard, nil
}

func (s *recordingSentinel) Acquire(_ context.Context, resource capsentinel.Resource) (capsentinel.Permit, error) {
	appendOrder(s.order, "sentinel:acquire")
	s.resource = resource
	s.permit = &recordingSentinelPermit{order: s.order}
	return s.permit, nil
}

type recordingSentinelGuard struct {
	order   *[]string
	dones   int
	doneErr error
}

func (g *recordingSentinelGuard) Done(err error) {
	appendOrder(g.order, "sentinel:done")
	g.dones++
	g.doneErr = err
}

type recordingSentinelPermit struct {
	order    *[]string
	releases int
}

func (p *recordingSentinelPermit) Release() {
	appendOrder(p.order, "sentinel:release")
	p.releases++
}

type rejectingServerSentinel struct {
	err      error
	resource capsentinel.Resource
}

func (s *rejectingServerSentinel) Allow(_ context.Context, resource capsentinel.Resource) (capsentinel.Guard, error) {
	s.resource = resource
	return nil, s.err
}

func sentinelAttribute(resource capsentinel.Resource, key string) any {
	for _, attr := range resource.Attributes {
		if attr.Key == key {
			return attr.Value
		}
	}
	return nil
}

func appendOrder(order *[]string, event string) {
	if order != nil && event != "" {
		*order = append(*order, event)
	}
}
