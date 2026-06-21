package runtimehttp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	caphttpclient "github.com/nucleuskit/nucleus/cap/httpclient"
	logcap "github.com/nucleuskit/nucleus/cap/log"
	metriccap "github.com/nucleuskit/nucleus/cap/metric"
	capsentinel "github.com/nucleuskit/nucleus/cap/sentinel"
	captransport "github.com/nucleuskit/nucleus/cap/transport"
	coreerrors "github.com/nucleuskit/nucleus/core/errors"
)

const (
	transportMetadataHTTPMethod = "http.method"
	transportMetadataURLHost    = "url.host"
)

type transportTargetKey struct{}

type Client struct {
	client    *http.Client
	options   caphttpclient.Options
	transport http.RoundTripper
	breaker   capsentinel.Breaker
	limiter   capsentinel.Limiter
}

func NewClient(options ...caphttpclient.Option) *Client {
	values := caphttpclient.NewOptions(options...)
	transport := newHTTPTransport(values)
	return &Client{
		client: &http.Client{
			Timeout: values.Timeout,
		},
		options:   values,
		transport: transport,
	}
}

func (c *Client) WithSentinel(breaker capsentinel.Breaker, limiter capsentinel.Limiter) *Client {
	clone := *c
	clone.breaker = breaker
	clone.limiter = limiter
	return &clone
}

func (c *Client) Do(ctx context.Context, request caphttpclient.Request) (caphttpclient.Response, error) {
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = c.options.Timeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	requestURL, err := caphttpclient.JoinURL(c.options.BaseURL, request.URL)
	if err != nil {
		return caphttpclient.Response{}, err
	}
	retry := request.Retry
	if retry.MaxAttempts <= 0 && retry.Backoff <= 0 && len(retry.RetryStatus) == 0 {
		retry = c.options.Retry
	}
	attempts := retry.Attempts()
	method := requestMethod(request.Method)
	startedAt := time.Now()
	c.emit(ctx, caphttpclient.Event{
		Kind:        caphttpclient.EventRequestStarted,
		Method:      method,
		URL:         requestURL,
		MaxAttempts: attempts,
		StartedAt:   startedAt,
		Metadata:    caphttpclient.CloneMetadata(request.Metadata),
	})
	complete := func(response caphttpclient.Response, err error) (caphttpclient.Response, error) {
		c.emit(ctx, caphttpclient.Event{
			Kind:        caphttpclient.EventRequestCompleted,
			Method:      method,
			URL:         requestURL,
			Attempt:     response.Attempt,
			MaxAttempts: attempts,
			StatusCode:  response.StatusCode,
			StartedAt:   startedAt,
			Duration:    time.Since(startedAt),
			Error:       err,
			Metadata:    caphttpclient.CloneMetadata(request.Metadata),
		})
		return response, err
	}
	var lastResponse caphttpclient.Response
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		response, err := c.doOnce(ctx, request, requestURL, attempt, attempts)
		lastResponse, lastErr = response, err
		if !retry.ShouldRetry(response.StatusCode, err, attempt) {
			return complete(response, err)
		}
		if err := waitRetry(ctx, retry.Backoff); err != nil {
			return complete(response, err)
		}
	}
	return complete(lastResponse, lastErr)
}

func (c *Client) doOnce(ctx context.Context, request caphttpclient.Request, requestURL string, attempt int, attempts int) (response caphttpclient.Response, err error) {
	method := requestMethod(request.Method)
	startedAt := time.Now()
	c.emit(ctx, caphttpclient.Event{
		Kind:        caphttpclient.EventAttemptStarted,
		Method:      method,
		URL:         requestURL,
		Attempt:     attempt,
		MaxAttempts: attempts,
		StartedAt:   startedAt,
		Metadata:    caphttpclient.CloneMetadata(request.Metadata),
	})
	defer func() {
		c.emit(ctx, caphttpclient.Event{
			Kind:        caphttpclient.EventAttemptCompleted,
			Method:      method,
			URL:         requestURL,
			Attempt:     attempt,
			MaxAttempts: attempts,
			StatusCode:  response.StatusCode,
			StartedAt:   startedAt,
			Duration:    time.Since(startedAt),
			Error:       err,
			Metadata:    caphttpclient.CloneMetadata(request.Metadata),
		})
	}()
	guard, permit, err := c.acquire(ctx, method, requestURL, request.Metadata)
	if err != nil {
		response = caphttpclient.Response{Attempt: attempt}
		return response, err
	}
	defer func() {
		if permit != nil {
			permit.Release()
		}
	}()

	ctx = c.withTransportTarget(ctx, method, requestURL, request.Metadata)
	req, err := http.NewRequestWithContext(ctx, method, requestURL, caphttpclient.CloneBody(request.Body))
	if err != nil {
		if guard != nil {
			guard.Done(err)
		}
		return response, err
	}
	req.Header = cloneHeader(c.options.Headers, request.Header)
	if request.ContentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", request.ContentType)
	}
	if c.options.UserAgent != "" && req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.options.UserAgent)
	}
	for _, header := range c.options.TraceNames {
		if req.Header.Get(header) == "" && request.Metadata != nil && request.Metadata[header] != "" {
			req.Header.Set(header, request.Metadata[header])
		}
	}

	httpClient := *c.client
	httpClient.Timeout = 0
	httpClient.Transport = c.transport
	resp, err := httpClient.Do(req)
	if err != nil {
		if guard != nil {
			guard.Done(breakerFailure(0, err))
		}
		response = caphttpclient.Response{Duration: time.Since(startedAt), Attempt: attempt}
		return response, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		if guard != nil {
			guard.Done(breakerFailure(resp.StatusCode, err))
		}
		response = caphttpclient.Response{StatusCode: resp.StatusCode, Duration: time.Since(startedAt), Attempt: attempt}
		return response, err
	}
	response = caphttpclient.Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header.Clone(),
		Body:       body,
		Duration:   time.Since(startedAt),
		Attempt:    attempt,
	}
	if resp.StatusCode >= http.StatusBadRequest {
		err = coreerrors.New(statusCode(resp.StatusCode), http.StatusText(resp.StatusCode))
		if guard != nil {
			guard.Done(breakerFailure(resp.StatusCode, nil))
		}
		return response, err
	}
	if guard != nil {
		guard.Done(nil)
	}
	return response, nil
}

func HTTPClientLogHook(logger logcap.Logger) caphttpclient.Hook {
	if logger == nil {
		return nil
	}
	return caphttpclient.HookFunc(func(ctx context.Context, event caphttpclient.Event) error {
		if event.Kind != caphttpclient.EventRequestCompleted {
			return nil
		}
		fields := []logcap.Field{
			logcap.String(logcap.FieldMethod, event.Method),
			logcap.String("url", event.URL),
			logcap.Int("attempt", event.Attempt),
			logcap.Int(logcap.FieldStatusCode, event.StatusCode),
			logcap.Duration(logcap.FieldDuration, event.Duration),
		}
		for key, value := range event.Metadata {
			fields = append(fields, logcap.String("metadata."+key, value))
		}
		if event.Error != nil {
			fields = append(fields, logcap.Error(event.Error))
			logger.Error(ctx, "http client request completed", fields...)
			return nil
		}
		logger.Info(ctx, "http client request completed", fields...)
		return nil
	})
}

func HTTPClientMetricHook(meter metriccap.Meter) caphttpclient.Hook {
	if meter == nil {
		return nil
	}
	requests := meter.Counter("http.client.requests")
	duration := meter.Gauge("http.client.duration_ms")
	return caphttpclient.HookFunc(func(ctx context.Context, event caphttpclient.Event) error {
		if event.Kind != caphttpclient.EventRequestCompleted {
			return nil
		}
		attrs := []metriccap.Attribute{
			metriccap.String("method", event.Method),
			metriccap.Any("status", event.StatusCode),
			metriccap.Bool("error", event.Error != nil),
		}
		for key, value := range event.Metadata {
			attrs = append(attrs, metriccap.String("metadata."+key, value))
		}
		if requests != nil {
			requests.Add(ctx, 1, attrs...)
		}
		if duration != nil {
			duration.Record(ctx, float64(event.Duration.Microseconds())/1000, attrs...)
		}
		return nil
	})
}

func (c *Client) Timeout() time.Duration {
	return c.options.Timeout
}

func newHTTPTransport(options caphttpclient.Options) http.RoundTripper {
	if options.TransportDialer == nil {
		return http.DefaultTransport
	}
	dialContext := func(ctx context.Context, network, address string) (net.Conn, error) {
		target := transportTargetFromContext(ctx)
		target.Network = captransport.DefaultNetwork(network)
		target.Address = address
		if options.TransportTargetPolicy != nil {
			next, err := options.TransportTargetPolicy(ctx, target.Clone())
			if err != nil {
				return nil, err
			}
			target = next.Clone()
			if target.Network == "" {
				target.Network = captransport.DefaultNetwork(network)
			}
			if target.Address == "" {
				target.Address = address
			}
		}
		return options.TransportDialer.DialContext(ctx, target.Clone())
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{
			Proxy:       http.ProxyFromEnvironment,
			DialContext: dialContext,
		}
	}
	transport := base.Clone()
	transport.DialContext = dialContext
	return transport
}

func (c *Client) withTransportTarget(ctx context.Context, method, requestURL string, metadata map[string]string) context.Context {
	if c.options.TransportDialer == nil {
		return ctx
	}
	target := captransport.Target{
		Metadata: captransport.CloneMetadata(metadata),
	}
	if target.Metadata == nil {
		target.Metadata = captransport.Metadata{}
	}
	target.Metadata[transportMetadataHTTPMethod] = method
	if parsed, err := url.Parse(requestURL); err == nil {
		if parsed.Host != "" {
			target.Metadata[transportMetadataURLHost] = parsed.Host
			target.ServerName = parsed.Hostname()
		}
	}
	return context.WithValue(ctx, transportTargetKey{}, target.Clone())
}

func transportTargetFromContext(ctx context.Context) captransport.Target {
	if ctx == nil {
		return captransport.Target{}
	}
	target, _ := ctx.Value(transportTargetKey{}).(captransport.Target)
	return target.Clone()
}

func (c *Client) emit(ctx context.Context, event caphttpclient.Event) {
	for _, hook := range c.options.Hooks {
		if hook == nil {
			continue
		}
		_ = hook.HandleHTTPClientEvent(ctx, event.Clone())
	}
}

func requestMethod(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return method
}

func breakerFailure(statusCode int, err error) error {
	if err != nil {
		return err
	}
	if statusCode >= http.StatusInternalServerError {
		statusText := http.StatusText(statusCode)
		if statusText == "" {
			statusText = "server error"
		}
		return fmt.Errorf("http status %d: %s", statusCode, statusText)
	}
	return nil
}

func (c *Client) acquire(ctx context.Context, method, requestURL string, metadata map[string]string) (capsentinel.Guard, capsentinel.Permit, error) {
	resourceName := method + " " + requestURL
	if metadata != nil && metadata["sentinel.resource"] != "" {
		resourceName = metadata["sentinel.resource"]
	}
	resource := capsentinel.Resource{Name: resourceName, Attributes: []capsentinel.Attribute{
		capsentinel.String("component", "httpclient"),
		capsentinel.String("method", method),
		capsentinel.String("url", requestURL),
	}}
	var guard capsentinel.Guard
	var permit capsentinel.Permit
	var err error
	if c.breaker != nil {
		guard, err = c.breaker.Allow(ctx, resource)
		if err != nil {
			return nil, nil, err
		}
	}
	if c.limiter != nil {
		permit, err = c.limiter.Acquire(ctx, resource)
		if err != nil {
			if guard != nil {
				guard.Done(err)
			}
			return nil, nil, err
		}
	}
	return guard, permit, nil
}

func cloneHeader(defaults map[string]string, request http.Header) http.Header {
	header := make(http.Header, len(defaults)+len(request))
	for key, value := range defaults {
		header.Set(key, value)
	}
	for key, values := range request {
		copied := append([]string(nil), values...)
		header[key] = copied
	}
	return header
}

func waitRetry(ctx context.Context, backoff time.Duration) error {
	if backoff <= 0 {
		return nil
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func statusCode(status int) coreerrors.Code {
	switch {
	case status == http.StatusNotFound:
		return coreerrors.CodeNotFound
	case status == http.StatusUnauthorized:
		return coreerrors.CodeUnauthenticated
	case status == http.StatusForbidden:
		return coreerrors.CodePermissionDenied
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return coreerrors.CodeDeadlineExceeded
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return coreerrors.CodeInvalidArgument
	case status >= http.StatusInternalServerError:
		return coreerrors.CodeUnavailable
	default:
		return coreerrors.CodeInternal
	}
}
