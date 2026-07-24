package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/thevilledev/sonnetbox"
)

func TestRender(t *testing.T) {
	app := newTestApplication(t)
	source := `local catalog = import "../lib/catalog.libsonnet";
std.trace("rendering " + std.extVar("customer"), {
  customer: std.extVar("customer"),
  product: catalog("starter"),
})`
	response := performRequest(t, app, http.MethodPost, "/render", renderBody(t, source))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}

	var got renderResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var output struct {
		Customer string `json:"customer"`
		Product  struct {
			Name         string `json:"name"`
			MonthlyPrice int    `json:"monthly_price"`
		} `json:"product"`
	}
	if err := json.Unmarshal(got.Output, &output); err != nil {
		t.Fatalf("decode rendered output: %v", err)
	}
	if output.Customer != "acme" ||
		output.Product.Name != "Starter" ||
		output.Product.MonthlyPrice != 9 {
		t.Fatalf("unexpected output: %+v", output)
	}
	if !strings.Contains(got.Trace, "rendering acme") {
		t.Fatalf("trace = %q, want rendering message", got.Trace)
	}
	if got.Stats.ImportResolutions != 1 ||
		got.Stats.CapabilityCalls != 1 ||
		got.Stats.TraceBytes == 0 ||
		got.Stats.TraceTruncated {
		t.Fatalf("unexpected stats: %+v", got.Stats)
	}
}

func TestRequestValidation(t *testing.T) {
	app := newTestApplication(t)
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown route",
			method:     http.MethodPost,
			path:       "/missing",
			body:       `{}`,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "wrong method",
			method:     http.MethodGet,
			path:       "/render",
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
		{
			name:       "malformed JSON",
			method:     http.MethodPost,
			path:       "/render",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:       "missing source",
			method:     http.MethodPost,
			path:       "/render",
			body:       `{"customer":"acme"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "invalid_request",
		},
		{
			name:   "body too large",
			method: http.MethodPost,
			path:   "/render",
			body: `{"source":"` +
				strings.Repeat("x", maxRequestBodyBytes+1) +
				`","customer":"acme"}`,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "request_too_large",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRequest(t, app, test.method, test.path, test.body)
			assertErrorResponse(t, response, test.wantStatus, test.wantCode)
		})
	}
}

func TestEvaluationErrors(t *testing.T) {
	app := newTestApplication(t)
	tests := []struct {
		name       string
		source     string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "denied import",
			source:     `import "../../secret.jsonnet"`,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "import_denied",
		},
		{
			name:       "evaluation failure",
			source:     `error "broken"`,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "evaluation_failed",
		},
		{
			name:       "output limit",
			source:     `"` + strings.Repeat("x", 9<<10) + `"`,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "limit_exceeded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performRequest(
				t,
				app,
				http.MethodPost,
				"/render",
				renderBody(t, test.source),
			)
			assertErrorResponse(t, response, test.wantStatus, test.wantCode)
		})
	}
}

func TestClassifyEvaluationError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{
			name:       "limit",
			err:        &sonnetbox.LimitError{},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "limit_exceeded",
		},
		{
			name:       "timeout",
			err:        &sonnetbox.CancellationError{Err: context.DeadlineExceeded},
			wantStatus: http.StatusGatewayTimeout,
			wantCode:   "evaluation_timeout",
		},
		{
			name:       "invalid evaluation request",
			err:        &sonnetbox.InvalidRequestError{Err: errors.New("invalid")},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "invalid_evaluation_request",
		},
		{
			name:       "trusted host failure",
			err:        errors.New("failed"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, code := classifyEvaluationError(
				fmt.Errorf("wrapped: %w", test.err),
			)
			if status != test.wantStatus || code != test.wantCode {
				t.Fatalf(
					"classification = (%d, %q), want (%d, %q)",
					status,
					code,
					test.wantStatus,
					test.wantCode,
				)
			}
		})
	}
}

func newTestApplication(t *testing.T) *application {
	t.Helper()
	engine, err := sonnetbox.NewEngine(t.Context(), sonnetbox.EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("close engine: %v", err)
		}
	})
	app, err := newApplication(engine)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func renderBody(t *testing.T, source string) string {
	t.Helper()
	body, err := json.Marshal(renderRequest{
		Source:   source,
		Customer: "acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func performRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertErrorResponse(
	t *testing.T,
	response *httptest.ResponseRecorder,
	wantStatus int,
	wantCode string,
) {
	t.Helper()
	if response.Code != wantStatus {
		t.Fatalf(
			"status = %d, want %d; body = %s",
			response.Code,
			wantStatus,
			response.Body.String(),
		)
	}
	var got errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if got.Code != wantCode || got.Error == "" {
		t.Fatalf("error response = %+v, want code %q and a message", got, wantCode)
	}
}
