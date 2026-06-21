package runtimehttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"

	nucleuscontext "github.com/nucleuskit/core/context"
	"github.com/nucleuskit/core/inbound"
)

const (
	inboundMetadataEndpoint  = "http.endpoint"
	inboundMetadataMethod    = "http.method"
	inboundMetadataOperation = "http.operation"
	inboundMetadataPath      = "http.path"
	inboundMetadataRoute     = "http.route"
)

type routeContextKey struct{}

type routeMetadata struct {
	Method    string
	Path      string
	Operation string
	Features  map[string]bool
}

func NewInboundRequest(request *http.Request) (inbound.Request, error) {
	if request == nil {
		return inbound.Request{}, errors.New("nil http request")
	}
	body, err := readAndRestoreBody(request)
	if err != nil {
		return inbound.Request{}, err
	}
	metadata := metadataFromHTTPHeader(request.Header)
	metadataFromContext(request.Context(), metadata)
	route := inboundRoute(request)
	metadataFromRoute(request, route, metadata)
	return inbound.Request{
		Kind:  inbound.KindHTTP,
		Route: route,
		Body: inbound.Body{
			ContentType: request.Header.Get("Content-Type"),
			Bytes:       body,
		},
		Metadata: metadata,
	}, nil
}

func withRouteContext(ctx context.Context, route Route) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, routeContextKey{}, routeMetadata{
		Method:    route.Method,
		Path:      route.Path,
		Operation: route.OperationID,
		Features:  cloneRouteFeatures(route.Features),
	})
}

func routeContext(ctx context.Context) (routeMetadata, bool) {
	if ctx == nil {
		return routeMetadata{}, false
	}
	route, ok := ctx.Value(routeContextKey{}).(routeMetadata)
	route.Features = cloneRouteFeatures(route.Features)
	return route, ok
}

func inboundRoute(request *http.Request) inbound.Route {
	route := inbound.Route{
		Method: request.Method,
		Path:   request.URL.Path,
	}
	if registered, ok := routeContext(request.Context()); ok {
		if registered.Method != "" {
			route.Method = registered.Method
		}
		if registered.Path != "" {
			route.Path = registered.Path
		}
		route.Operation = registered.Operation
	}
	return route
}

func readAndRestoreBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func metadataFromHTTPHeader(header http.Header) inbound.Metadata {
	metadata := inbound.Metadata{}
	for key, values := range header {
		for _, value := range values {
			metadata.Add(key, value)
		}
	}
	setIfPresent(metadata, inbound.KeyTraceID, metadata.Get(inbound.HeaderTraceParent))
	setIfPresent(metadata, inbound.KeyRequestID, metadata.Get(inbound.HeaderRequestID))
	setIfPresent(metadata, inbound.KeyTenant, metadata.Get(inbound.HeaderTenant))
	return metadata
}

func metadataFromContext(ctx context.Context, metadata inbound.Metadata) {
	if metadata.Get(inbound.KeyTraceID) == "" {
		setIfPresent(metadata, inbound.KeyTraceID, nucleuscontext.TraceID(ctx))
	}
	if metadata.Get(inbound.KeyRequestID) == "" {
		setIfPresent(metadata, inbound.KeyRequestID, nucleuscontext.RequestID(ctx))
	}
	if metadata.Get(inbound.KeyTenant) == "" {
		setIfPresent(metadata, inbound.KeyTenant, nucleuscontext.Tenant(ctx))
	}
}

func metadataFromRoute(request *http.Request, route inbound.Route, metadata inbound.Metadata) {
	setIfPresent(metadata, inboundMetadataMethod, route.Method)
	setIfPresent(metadata, inboundMetadataPath, request.URL.Path)
	setIfPresent(metadata, inboundMetadataRoute, route.Path)
	setIfPresent(metadata, inboundMetadataOperation, route.Operation)
	if route.Method != "" && route.Path != "" {
		metadata.Set(inboundMetadataEndpoint, route.Method+" "+route.Path)
	}
}

func setIfPresent(metadata inbound.Metadata, key string, value string) {
	if value != "" {
		metadata.Set(key, value)
	}
}
