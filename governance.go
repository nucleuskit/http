package runtimehttp

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	coreerrors "github.com/nucleuskit/core/errors"
	"github.com/nucleuskit/core/response"
)

func MaxBytes(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			request.Body = http.MaxBytesReader(writer, request.Body, limit)
			next.ServeHTTP(writer, request)
		})
	}
}

func Gunzip() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if !strings.EqualFold(request.Header.Get("Content-Encoding"), "gzip") || request.Body == nil {
				next.ServeHTTP(writer, request)
				return
			}
			reader, err := gzip.NewReader(request.Body)
			if err != nil {
				writeInvalidBody(writer, request, "invalid gzip request body")
				return
			}
			request.Body = gzipBody{Reader: reader, gzip: reader, original: request.Body}
			request.Header.Del("Content-Encoding")
			next.ServeHTTP(writer, request)
		})
	}
}

func MaxConns(limit int) Middleware {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}
		sem := make(chan struct{}, limit)
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				next.ServeHTTP(writer, request)
			default:
				err := coreerrors.New(coreerrors.CodeUnavailable, coreerrors.CodeUnavailable.DefaultMessage())
				writeJSON(writer, http.StatusTooManyRequests, response.Error(err, traceID(request)))
			}
		})
	}
}

type gzipBody struct {
	io.Reader
	gzip     io.Closer
	original io.Closer
}

func (b gzipBody) Close() error {
	err := b.gzip.Close()
	if closeErr := b.original.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeInvalidBody(writer http.ResponseWriter, request *http.Request, message string) {
	err := coreerrors.New(coreerrors.CodeInvalidArgument, message)
	writeJSON(writer, coreerrors.HTTPStatus(coreerrors.CodeInvalidArgument), response.Error(err, traceID(request)))
}
