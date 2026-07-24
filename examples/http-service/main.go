// Command http-service exposes bounded Jsonnet evaluation over HTTP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/thevilledev/sonnetbox"
)

const (
	catalogLibrary      = `function(sku) std.native("lookup_product")(sku)`
	evaluationTimeout   = 5 * time.Second
	maxRequestBodyBytes = 128 << 10
)

type application struct {
	engine   *sonnetbox.Engine
	importer sonnetbox.Importer
	products map[string]map[string]any
}

type renderRequest struct {
	Source   string `json:"source"`
	Customer string `json:"customer"`
}

type renderResponse struct {
	Output json.RawMessage `json:"output"`
	Trace  string          `json:"trace,omitempty"`
	Stats  responseStats   `json:"stats"`
}

type responseStats struct {
	QueueDuration     string `json:"queue_duration"`
	ExecutionDuration string `json:"execution_duration"`
	FuelConsumed      uint64 `json:"fuel_consumed"`
	ImportResolutions uint32 `json:"import_resolutions"`
	ImportBytes       uint64 `json:"import_bytes"`
	CapabilityCalls   uint32 `json:"capability_calls"`
	TraceBytes        uint32 `json:"trace_bytes"`
	TraceTruncated    bool   `json:"trace_truncated"`
}

type errorResponse struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	logger := log.New(os.Stderr, "", log.LstdFlags)
	err := runServer(ctx, os.Args[1:], logger)
	stop()
	if err != nil {
		logger.Fatal(err)
	}
}

func newApplication(engine *sonnetbox.Engine) (*application, error) {
	importer, err := sonnetbox.NewMapImporter(map[string][]byte{
		"lib/catalog.libsonnet": []byte(catalogLibrary),
	})
	if err != nil {
		return nil, err
	}
	return &application{
		engine:   engine,
		importer: importer,
		products: map[string]map[string]any{
			"starter": {
				"name":          "Starter",
				"monthly_price": 9,
			},
			"scale": {
				"name":          "Scale",
				"monthly_price": 49,
			},
		},
	}, nil
}

func (app *application) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/render" {
		respondJSON(writer, http.StatusNotFound, errorResponse{
			Code:  "not_found",
			Error: "route not found",
		})
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		respondJSON(writer, http.StatusMethodNotAllowed, errorResponse{
			Code:  "method_not_allowed",
			Error: "use POST /render",
		})
		return
	}

	input, err := decodeRenderRequest(writer, request)
	if err != nil {
		status := http.StatusBadRequest
		code := "invalid_request"
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			status = http.StatusRequestEntityTooLarge
			code = "request_too_large"
		}
		respondJSON(writer, status, errorResponse{Code: code, Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(request.Context(), evaluationTimeout)
	defer cancel()
	result, err := app.engine.Evaluate(ctx, sonnetbox.Request{
		Filename:     "requests/input.jsonnet",
		Source:       input.Source,
		ExtVars:      map[string]string{"customer": input.Customer},
		Importer:     app.importer,
		Capabilities: app.capabilities(),
		Limits:       serviceRequestLimits(),
		CaptureTrace: true,
	})
	if err != nil {
		status, code := classifyEvaluationError(err)
		message := err.Error()
		if status == http.StatusInternalServerError {
			message = "internal evaluation error"
		}
		respondJSON(writer, status, errorResponse{Code: code, Error: message})
		return
	}

	respondJSON(writer, http.StatusOK, renderResponse{
		Output: json.RawMessage(result.Output),
		Trace:  string(result.Trace),
		Stats:  newResponseStats(result.Stats),
	})
}

func (app *application) capabilities() map[string]sonnetbox.Capability {
	return map[string]sonnetbox.Capability{
		"lookup_product": {
			Params: []string{"sku"},
			Call: func(ctx context.Context, args []any) (any, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				sku, ok := args[0].(string)
				if !ok {
					return nil, nil
				}
				product, ok := app.products[sku]
				if !ok {
					return nil, nil
				}
				return maps.Clone(product), nil
			},
		},
	}
}

func serviceRequestLimits() sonnetbox.RequestLimits {
	return sonnetbox.RequestLimits{
		MaxFuel:             50_000_000,
		MaxSourceBytes:      64 << 10,
		MaxOutputBytes:      8 << 10,
		MaxImports:          8,
		MaxImportBytes:      32 << 10,
		MaxTotalImportBytes: 128 << 10,
		MaxCapabilityCalls:  16,
		MaxTraceBytes:       8 << 10,
	}
}

func decodeRenderRequest(
	writer http.ResponseWriter,
	request *http.Request,
) (renderRequest, error) {
	body := http.MaxBytesReader(writer, request.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	var input renderRequest
	if err := decoder.Decode(&input); err != nil {
		return renderRequest{}, fmt.Errorf("decode request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return renderRequest{}, errors.New("request must contain one JSON object")
		}
		return renderRequest{}, fmt.Errorf("decode trailing request data: %w", err)
	}
	if strings.TrimSpace(input.Source) == "" {
		return renderRequest{}, errors.New("source is required")
	}
	if strings.TrimSpace(input.Customer) == "" {
		return renderRequest{}, errors.New("customer is required")
	}
	return input, nil
}

func classifyEvaluationError(err error) (int, string) {
	var limitError *sonnetbox.LimitError
	if errors.As(err, &limitError) {
		return http.StatusRequestEntityTooLarge, "limit_exceeded"
	}
	var cancellationError *sonnetbox.CancellationError
	if errors.As(err, &cancellationError) {
		return http.StatusGatewayTimeout, "evaluation_timeout"
	}
	var importDeniedError *sonnetbox.ImportDeniedError
	if errors.As(err, &importDeniedError) {
		return http.StatusUnprocessableEntity, "import_denied"
	}
	var evaluationError *sonnetbox.EvaluationError
	if errors.As(err, &evaluationError) {
		return http.StatusUnprocessableEntity, "evaluation_failed"
	}
	var invalidRequestError *sonnetbox.InvalidRequestError
	if errors.As(err, &invalidRequestError) {
		return http.StatusUnprocessableEntity, "invalid_evaluation_request"
	}
	return http.StatusInternalServerError, "internal_error"
}

func newResponseStats(stats sonnetbox.EvaluationStats) responseStats {
	return responseStats{
		QueueDuration:     stats.QueueDuration.String(),
		ExecutionDuration: stats.ExecutionDuration.String(),
		FuelConsumed:      stats.FuelConsumed,
		ImportResolutions: stats.ImportResolutions,
		ImportBytes:       stats.ImportBytes,
		CapabilityCalls:   stats.CapabilityCalls,
		TraceBytes:        stats.TraceBytes,
		TraceTruncated:    stats.TraceTruncated,
	}
}

func respondJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		log.Printf("write JSON response: %v", err)
	}
}

func runServer(
	ctx context.Context,
	args []string,
	logger *log.Logger,
) (runErr error) {
	flags := flag.NewFlagSet("http-service", flag.ContinueOnError)
	flags.SetOutput(logger.Writer())
	address := flags.String("addr", "127.0.0.1:8080", "HTTP listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*address) == "" {
		return errors.New("-addr must not be empty")
	}

	engine, err := sonnetbox.NewEngine(ctx, sonnetbox.EngineConfig{})
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, engine.Close(context.WithoutCancel(ctx)))
	}()

	app, err := newApplication(engine)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	logger.Printf("listening on http://%s", *address)
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			5*time.Second,
		)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownCtx)
		serverErr := <-serverErrors
		if errors.Is(serverErr, http.ErrServerClosed) {
			serverErr = nil
		}
		return errors.Join(shutdownErr, serverErr)
	}
}
