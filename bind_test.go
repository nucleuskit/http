package runtimehttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	coreerrors "github.com/nucleuskit/nucleus/core/errors"
)

func TestBindQueryParamRequiresValue(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/users", nil)

	_, err := BindQueryParam(request, "limit")

	assertInvalidArgument(t, err)
}

func TestBindPathParamReadsServeMuxValue(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	request.SetPathValue("id", "42")

	value, err := BindPathParam(request, "id")
	if err != nil {
		t.Fatal(err)
	}
	if value != "42" {
		t.Fatalf("expected path id 42, got %q", value)
	}
}

func TestBindJSONMapsMalformedBodyToInvalidArgument(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`{"name":`))
	var payload struct {
		Name string `json:"name"`
	}

	err := BindJSON(request, &payload)

	assertInvalidArgument(t, err)
}

func TestJSONDecoderImplementsRequestDecoder(t *testing.T) {
	var decoder RequestDecoder = JSONDecoder{}
	request := httptest.NewRequest(http.MethodPost, "/users", strings.NewReader(`{"name":"annie"}`))
	var payload struct {
		Name string `json:"name"`
	}

	if err := decoder.DecodeHTTPRequest(request, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Name != "annie" {
		t.Fatalf("unexpected decoded payload: %#v", payload)
	}
}

func TestBindQueryStructSupportsPrimitiveSlices(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/orders?status=paid&status=pending&limit=20&active=true", nil)
	var payload struct {
		Status []string `query:"status"`
		Limit  int      `query:"limit"`
		Active bool     `query:"active"`
	}

	if err := BindQuery(request, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Status) != 2 || payload.Status[0] != "paid" || payload.Status[1] != "pending" {
		t.Fatalf("unexpected status: %#v", payload.Status)
	}
	if payload.Limit != 20 || !payload.Active {
		t.Fatalf("unexpected scalar values: %#v", payload)
	}
}

func TestBindFormStructSupportsPrimitiveSlices(t *testing.T) {
	body := url.Values{
		"name":  []string{"coffee"},
		"ids":   []string{"1", "2"},
		"dry":   []string{"false"},
		"count": []string{"3"},
	}.Encode()
	request := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var payload struct {
		Name  string `form:"name"`
		IDs   []int  `form:"ids"`
		Dry   bool   `form:"dry"`
		Count int64  `form:"count"`
	}

	if err := BindForm(request, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Name != "coffee" || len(payload.IDs) != 2 || payload.IDs[0] != 1 || payload.IDs[1] != 2 || payload.Dry || payload.Count != 3 {
		t.Fatalf("unexpected form payload: %#v", payload)
	}
}

func TestBindStructMapsInvalidValuesToInvalidArgument(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/orders?limit=bad", nil)
	var payload struct {
		Limit int `query:"limit"`
	}

	err := BindQuery(request, &payload)

	assertInvalidArgument(t, err)
}

func assertInvalidArgument(t *testing.T, err error) {
	t.Helper()
	var codeErr *coreerrors.CodeError
	if !errors.As(err, &codeErr) {
		t.Fatalf("expected CodeError, got %T %v", err, err)
	}
	if codeErr.Code != coreerrors.CodeInvalidArgument {
		t.Fatalf("expected invalid argument, got %d", codeErr.Code)
	}
}
