package runtimehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	caphttpclient "github.com/nucleuskit/cap/httpclient"
	capsentinel "github.com/nucleuskit/cap/sentinel"
	captransport "github.com/nucleuskit/cap/transport"
	coreerrors "github.com/nucleuskit/core/errors"
)

func TestClientSendsRequestAndPropagatesTraceHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", request.Method)
		}
		if request.Header.Get("traceparent") != "trace-1" {
			t.Fatalf("expected traceparent trace-1, got %q", request.Header.Get("traceparent"))
		}
		writer.Header().Set("X-Reply", "ok")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte("created"))
	}))
	defer server.Close()

	client := NewClient(caphttpclient.WithUserAgent("nucleus-test"))
	response, err := client.Do(context.Background(), caphttpclient.Request{
		Method: http.MethodPost,
		URL:    server.URL,
		Header: http.Header{"traceparent": []string{"trace-1"}},
		Body:   []byte("payload"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", response.StatusCode)
	}
	if string(response.Body) != "created" {
		t.Fatalf("expected created body, got %q", string(response.Body))
	}
	if response.Header.Get("X-Reply") != "ok" {
		t.Fatalf("expected reply header ok, got %q", response.Header.Get("X-Reply"))
	}
}

func TestClientUsesPerRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(50 * time.Millisecond)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(caphttpclient.WithTimeout(time.Second))
	_, err := client.Do(context.Background(), caphttpclient.Request{
		Method:  http.MethodGet,
		URL:     server.URL,
		Timeout: time.Nanosecond,
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestClientMapsHTTPErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte("bad gateway"))
	}))
	defer server.Close()

	client := NewClient()
	response, err := client.Do(context.Background(), caphttpclient.Request{
		Method: http.MethodGet,
		URL:    server.URL,
	})
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected status 502, got %d", response.StatusCode)
	}
	var codeErr *coreerrors.CodeError
	if !errors.As(err, &codeErr) {
		t.Fatalf("expected CodeError, got %T %v", err, err)
	}
	if codeErr.Code != coreerrors.CodeUnavailable {
		t.Fatalf("expected unavailable code, got %d", codeErr.Code)
	}
}

func TestClientRetriesAndRecordsAttempt(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()

	client := NewClient(caphttpclient.WithRetry(caphttpclient.RetryPolicy{MaxAttempts: 2}))
	response, err := client.Do(context.Background(), caphttpclient.Request{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if response.Attempt != 2 {
		t.Fatalf("expected second attempt, got %d", response.Attempt)
	}
	if string(response.Body) != "ok" {
		t.Fatalf("expected ok body, got %q", string(response.Body))
	}
}

func TestClientHooksObserveRequestAndAttempts(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()

	var events []caphttpclient.Event
	var hookOrder []string
	client := NewClient(
		caphttpclient.WithRetry(caphttpclient.RetryPolicy{MaxAttempts: 2}),
		caphttpclient.WithHooks(caphttpclient.HookFunc(func(ctx context.Context, event caphttpclient.Event) error {
			events = append(events, event)
			hookOrder = append(hookOrder, "first:"+string(event.Kind))
			return errors.New("hook failure should be ignored")
		}), caphttpclient.HookFunc(func(ctx context.Context, event caphttpclient.Event) error {
			hookOrder = append(hookOrder, "second:"+string(event.Kind))
			return nil
		})),
	)
	response, err := client.Do(context.Background(), caphttpclient.Request{
		Method:   http.MethodPost,
		URL:      server.URL,
		Metadata: map[string]string{"route": "orders.create"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Attempt != 2 {
		t.Fatalf("expected second attempt, got %d", response.Attempt)
	}

	kinds := make([]caphttpclient.EventKind, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, event.Kind)
		if event.Method != http.MethodPost || event.URL != server.URL {
			t.Fatalf("unexpected event request identity: %#v", event)
		}
		if event.Metadata["route"] != "orders.create" {
			t.Fatalf("expected cloned request metadata, got %#v", event.Metadata)
		}
	}
	wantKinds := []caphttpclient.EventKind{
		caphttpclient.EventRequestStarted,
		caphttpclient.EventAttemptStarted,
		caphttpclient.EventAttemptCompleted,
		caphttpclient.EventAttemptStarted,
		caphttpclient.EventAttemptCompleted,
		caphttpclient.EventRequestCompleted,
	}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("expected events %#v, got %#v", wantKinds, kinds)
	}
	for index := range wantKinds {
		if kinds[index] != wantKinds[index] {
			t.Fatalf("expected events %#v, got %#v", wantKinds, kinds)
		}
	}
	if events[2].Attempt != 1 || events[2].StatusCode != http.StatusBadGateway || events[2].Error == nil {
		t.Fatalf("expected failed first attempt event, got %#v", events[2])
	}
	if events[4].Attempt != 2 || events[4].StatusCode != http.StatusOK || events[4].Error != nil {
		t.Fatalf("expected successful second attempt event, got %#v", events[4])
	}
	if events[5].Attempt != 2 || events[5].StatusCode != http.StatusOK || events[5].Duration <= 0 {
		t.Fatalf("expected completed request event, got %#v", events[5])
	}
	if hookOrder[0] != "first:request_started" || hookOrder[1] != "second:request_started" {
		t.Fatalf("expected hooks in registration order, got %#v", hookOrder[:2])
	}
}

func TestClientHooksObserveNetworkError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	var events []caphttpclient.Event
	client := NewClient(caphttpclient.WithHooks(caphttpclient.HookFunc(func(ctx context.Context, event caphttpclient.Event) error {
		events = append(events, event.Clone())
		return nil
	})))
	_, err = client.Do(context.Background(), caphttpclient.Request{URL: url})
	if err == nil {
		t.Fatal("expected network error")
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d: %#v", len(events), events)
	}
	if events[2].Kind != caphttpclient.EventAttemptCompleted || events[2].Error == nil || events[2].StatusCode != 0 {
		t.Fatalf("expected failed attempt event, got %#v", events[2])
	}
	if events[3].Kind != caphttpclient.EventRequestCompleted || events[3].Error == nil {
		t.Fatalf("expected failed request event, got %#v", events[3])
	}
}

func TestHTTPClientLogAndMetricHooks(t *testing.T) {
	logger := &recordingLogger{}
	meter := newRecordingMeter()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	client := NewClient(
		caphttpclient.WithHooks(HTTPClientLogHook(logger), HTTPClientMetricHook(meter)),
	)
	response, err := client.Do(context.Background(), caphttpclient.Request{Method: http.MethodPut, URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", response.StatusCode)
	}
	if logger.infoCalls != 1 {
		t.Fatalf("expected one log call, got %d", logger.infoCalls)
	}
	if meter.counters["http.client.requests"].adds != 1 {
		t.Fatalf("expected request counter add, got %d", meter.counters["http.client.requests"].adds)
	}
	if meter.gauges["http.client.duration_ms"].records != 1 {
		t.Fatalf("expected duration record, got %d", meter.gauges["http.client.duration_ms"].records)
	}
}

func TestClientUsesTransportDialerTargetMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	dialer := &recordingDialer{}
	client := NewClient(
		caphttpclient.WithTransportDialer(dialer),
		caphttpclient.WithTransportTargetPolicy(func(ctx context.Context, target captransport.Target) (captransport.Target, error) {
			target.Metadata = captransport.MergeMetadata(target.Metadata, map[string]string{"policy": "applied"})
			return target, nil
		}),
	)
	response, err := client.Do(context.Background(), caphttpclient.Request{
		Method:   http.MethodPost,
		URL:      server.URL,
		Metadata: map[string]string{"route": "orders.create"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", response.StatusCode)
	}

	targets := dialer.Targets()
	if len(targets) != 1 {
		t.Fatalf("expected one dial target, got %d", len(targets))
	}
	target := targets[0]
	if target.Network == "" || target.Address == "" {
		t.Fatalf("expected network and address, got %#v", target)
	}
	if target.ServerName != serverURL.Hostname() {
		t.Fatalf("expected server name %q, got %q", serverURL.Hostname(), target.ServerName)
	}
	if target.Metadata[transportMetadataHTTPMethod] != http.MethodPost {
		t.Fatalf("expected method metadata, got %#v", target.Metadata)
	}
	if target.Metadata[transportMetadataURLHost] != serverURL.Host {
		t.Fatalf("expected url host metadata, got %#v", target.Metadata)
	}
	if target.Metadata["route"] != "orders.create" || target.Metadata["policy"] != "applied" {
		t.Fatalf("expected request and policy metadata, got %#v", target.Metadata)
	}
}

func TestClientDefaultTransportUnchangedWithoutTransportDialer(t *testing.T) {
	client := NewClient()
	if client.transport != http.DefaultTransport {
		t.Fatalf("expected default transport without cap transport dialer")
	}
}

func TestClientRetryKeepsTransportTargetMetadataStable(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			writer.Header().Set("Connection", "close")
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	dialer := &recordingDialer{mutateMetadata: true}
	metadata := map[string]string{"route": "orders.list"}
	client := NewClient(
		caphttpclient.WithTransportDialer(dialer),
		caphttpclient.WithRetry(caphttpclient.RetryPolicy{MaxAttempts: 2}),
	)
	response, err := client.Do(context.Background(), caphttpclient.Request{
		URL:      server.URL,
		Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Attempt != 2 {
		t.Fatalf("expected second attempt, got %d", response.Attempt)
	}
	if metadata["route"] != "orders.list" {
		t.Fatalf("request metadata was mutated: %#v", metadata)
	}

	targets := dialer.Targets()
	if len(targets) != 2 {
		t.Fatalf("expected two dial targets, got %d", len(targets))
	}
	for index, target := range targets {
		if target.Metadata["route"] != "orders.list" {
			t.Fatalf("target %d route metadata was mutated: %#v", index, target.Metadata)
		}
		if target.Metadata["dial.mutated"] != "" {
			t.Fatalf("target %d leaked dialer mutation: %#v", index, target.Metadata)
		}
		if target.Metadata[transportMetadataHTTPMethod] != http.MethodGet || target.Metadata[transportMetadataURLHost] != serverURL.Host {
			t.Fatalf("target %d lost request metadata: %#v", index, target.Metadata)
		}
	}
}

func TestClientAppliesBaseURLDefaultHeadersAndTraceMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/orders" {
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
		if request.Header.Get("X-App") != "nucleus" {
			t.Fatalf("expected default header")
		}
		if request.Header.Get("traceparent") != "trace-1" {
			t.Fatalf("expected trace metadata header")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewClient(
		caphttpclient.WithBaseURL(server.URL+"/v1/"),
		caphttpclient.WithHeader("X-App", "nucleus"),
		caphttpclient.WithTraceHeader("traceparent"),
	)
	response, err := client.Do(context.Background(), caphttpclient.Request{
		URL:      "orders",
		Metadata: map[string]string{"traceparent": "trace-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", response.StatusCode)
	}
}

func TestClientSendsStructQueryAndFormRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/search":
			if request.URL.Query().Get("q") != "coffee" || request.URL.Query().Get("limit") != "2" {
				t.Fatalf("unexpected query: %s", request.URL.RawQuery)
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]string{"ok": "query"})
		case "/submit":
			if request.Header.Get("Content-Type") != caphttpclient.ContentTypeForm {
				t.Fatalf("expected form content type, got %q", request.Header.Get("Content-Type"))
			}
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if request.PostForm.Get("name") != "coffee" || request.PostForm.Get("count") != "3" {
				t.Fatalf("unexpected form body: %#v", request.PostForm)
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]string{"ok": "form"})
		default:
			t.Fatalf("unexpected path %s", request.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(caphttpclient.WithBaseURL(server.URL))
	queryRequest, err := caphttpclient.Query(http.MethodGet, "/search", struct {
		Term  string `query:"q"`
		Limit int    `query:"limit"`
	}{Term: "coffee", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	queryResponse, err := client.Do(context.Background(), queryRequest)
	if err != nil {
		t.Fatal(err)
	}
	var queryPayload map[string]string
	if err := queryResponse.DecodeJSON(&queryPayload); err != nil {
		t.Fatal(err)
	}
	if queryPayload["ok"] != "query" {
		t.Fatalf("unexpected query response: %#v", queryPayload)
	}

	formRequest, err := caphttpclient.FormStruct(http.MethodPost, "/submit", struct {
		Name  string `form:"name"`
		Count int    `form:"count"`
	}{Name: "coffee", Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	formResponse, err := client.Do(context.Background(), formRequest)
	if err != nil {
		t.Fatal(err)
	}
	var formPayload map[string]string
	if err := formResponse.DecodeJSON(&formPayload); err != nil {
		t.Fatal(err)
	}
	if formPayload["ok"] != "form" {
		t.Fatalf("unexpected form response: %#v", formPayload)
	}
}

func TestClientUsesSentinelGuard(t *testing.T) {
	errRejected := errors.New("rejected")
	client := NewClient().WithSentinel(rejectingBreaker{err: errRejected}, nil)

	_, err := client.Do(context.Background(), caphttpclient.Request{URL: "https://example.com"})
	if !errors.Is(err, errRejected) {
		t.Fatalf("expected sentinel rejection, got %v", err)
	}
}

func TestClientBreakerClassifiesHTTPStatusFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		statusCode int
		wantDone   bool
		wantCode   coreerrors.Code
	}{
		"4xx does not trip breaker": {
			statusCode: http.StatusNotFound,
			wantDone:   false,
			wantCode:   coreerrors.CodeNotFound,
		},
		"5xx trips breaker": {
			statusCode: http.StatusBadGateway,
			wantDone:   true,
			wantCode:   coreerrors.CodeUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(tc.statusCode)
			}))
			defer server.Close()

			breaker := &recordingClientBreaker{}
			response, err := NewClient().WithSentinel(breaker, nil).Do(context.Background(), caphttpclient.Request{URL: server.URL})
			if response.StatusCode != tc.statusCode {
				t.Fatalf("expected status %d, got %d", tc.statusCode, response.StatusCode)
			}
			var codeErr *coreerrors.CodeError
			if !errors.As(err, &codeErr) {
				t.Fatalf("expected CodeError, got %T %v", err, err)
			}
			if codeErr.Code != tc.wantCode {
				t.Fatalf("expected code %d, got %d", tc.wantCode, codeErr.Code)
			}
			if breaker.guard == nil {
				t.Fatal("expected breaker guard to be acquired")
			}
			gotDone := breaker.guard.doneErr != nil
			if gotDone != tc.wantDone {
				t.Fatalf("expected breaker done error=%v, got %v", tc.wantDone, breaker.guard.doneErr)
			}
		})
	}
}

func TestClientBreakerClassifiesTimeoutAndNetworkErrors(t *testing.T) {
	t.Run("timeout trips breaker", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			time.Sleep(50 * time.Millisecond)
			writer.WriteHeader(http.StatusNoContent)
		}))
		defer server.Close()

		breaker := &recordingClientBreaker{}
		_, err := NewClient().WithSentinel(breaker, nil).Do(context.Background(), caphttpclient.Request{
			URL:     server.URL,
			Timeout: time.Millisecond,
		})
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if breaker.guard == nil || breaker.guard.doneErr == nil {
			t.Fatalf("expected timeout to trip breaker, got %#v", breaker.guard)
		}
	})

	t.Run("network error trips breaker", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		requestURL := "http://" + listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}

		breaker := &recordingClientBreaker{}
		_, err = NewClient().WithSentinel(breaker, nil).Do(context.Background(), caphttpclient.Request{URL: requestURL})
		if err == nil {
			t.Fatal("expected network error")
		}
		if breaker.guard == nil || breaker.guard.doneErr == nil {
			t.Fatalf("expected network error to trip breaker, got %#v", breaker.guard)
		}
	})
}

type rejectingBreaker struct {
	err error
}

func (b rejectingBreaker) Allow(context.Context, capsentinel.Resource) (capsentinel.Guard, error) {
	return nil, b.err
}

type recordingClientBreaker struct {
	resource capsentinel.Resource
	guard    *recordingClientGuard
}

func (b *recordingClientBreaker) Allow(_ context.Context, resource capsentinel.Resource) (capsentinel.Guard, error) {
	b.resource = resource
	b.guard = &recordingClientGuard{}
	return b.guard, nil
}

type recordingClientGuard struct {
	doneErr error
}

func (g *recordingClientGuard) Done(err error) {
	g.doneErr = err
}

type recordingDialer struct {
	mu             sync.Mutex
	targets        []captransport.Target
	mutateMetadata bool
}

func (d *recordingDialer) DialContext(ctx context.Context, target captransport.Target) (net.Conn, error) {
	d.mu.Lock()
	d.targets = append(d.targets, target.Clone())
	d.mu.Unlock()
	if d.mutateMetadata && target.Metadata != nil {
		target.Metadata["route"] = "mutated"
		target.Metadata["dial.mutated"] = "true"
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, captransport.DefaultNetwork(target.Network), target.Address)
}

func (d *recordingDialer) Targets() []captransport.Target {
	d.mu.Lock()
	defer d.mu.Unlock()
	targets := make([]captransport.Target, len(d.targets))
	for index, target := range d.targets {
		targets[index] = target.Clone()
	}
	return targets
}
