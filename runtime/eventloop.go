package runtime

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gurre/lambda/log"
	"github.com/gurre/lambda/runtimeapi"
	jsoniter "github.com/json-iterator/go"
)

func init() {
	// Set GOMAXPROCS to NumCPU to prevent CPU contention on single vCPU environments
	// This helps ensure background work doesn't starve response marshalling
	runtime.GOMAXPROCS(runtime.NumCPU())
}

// ErrorResponse is a reusable struct for error responses
type ErrorResponse struct {
	ErrorMessage string `json:"errorMessage"`
	ErrorType    string `json:"errorType"`
}

// Note: We intentionally avoid custom buffer pooling for JSON marshaling.
// json-iterator marshals directly to []byte efficiently for our typical payload sizes.

type EventLoop[T, R any] struct {
	handler Handler[T, R]
	logger  log.Logger

	// Optimizations: Pre-allocated reusable objects
	errorResponse ErrorResponse
	eventBuffer   T

	// Additional optimizations: Pre-allocated buffers and pools
	jsoniter   jsoniter.API // Dedicated API instance
	bufferPool *sync.Pool   // Buffer pool for large payloads
	stringPool *sync.Pool   // String buffer pool
	timeBuffer time.Time    // Reusable time buffer
}

func NewEventLoop[T, R any](h Handler[T, R]) *EventLoop[T, R] {
	logger := log.New(log.LevelInfo, os.Stdout)

	// Create dedicated jsoniter API instance for this EventLoop
	jsonAPI := jsoniter.Config{
		EscapeHTML:                    false, // Faster for Lambda responses
		SortMapKeys:                   false, // Preserve insertion order for speed
		ValidateJsonRawMessage:        false, // Skip validation for performance
		UseNumber:                     false, // Use float64 directly
		MarshalFloatWith6Digits:       true,  // Reduce precision for speed
		ObjectFieldMustBeSimpleString: true,  // No unescaping overhead
	}.Froze()

	// Create buffer pools for large payloads
	bufferPool := &sync.Pool{
		New: func() interface{} {
			// Pre-allocate 4KB buffer for typical Lambda payloads
			return make([]byte, 0, 4096)
		},
	}

	stringPool := &sync.Pool{
		New: func() interface{} {
			// Pre-allocate strings.Builder with 1KB capacity
			builder := &strings.Builder{}
			builder.Grow(1024)
			return builder
		},
	}

	e := &EventLoop[T, R]{
		handler:       h,
		logger:        logger,
		errorResponse: ErrorResponse{},
		eventBuffer:   *new(T), // Zero value of type T

		// Performance optimizations
		jsoniter:   jsonAPI,
		bufferPool: bufferPool,
		stringPool: stringPool,
		timeBuffer: time.Time{}, // Zero time to reuse
	}

	return e
}

// Optimized JSON marshaling with buffer reuse and Content-Length optimization
func (e *EventLoop[T, R]) marshalWithPool(v interface{}) ([]byte, error) {
	// Use the dedicated jsoniter API instance for this EventLoop
	return e.jsoniter.Marshal(v)
}

// Optimized JSON unmarshaling with buffer reuse
func (e *EventLoop[T, R]) unmarshalWithPool(data []byte, v interface{}) error {
	return e.jsoniter.Unmarshal(data, v)
}

func (e *EventLoop[T, R]) Run(ctx context.Context) error {
	api, err := runtimeapi.NewClient()
	if err != nil {
		return err
	}

	// Create a context canceled on SIGTERM/SIGINT so handlers can observe cancellation
	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Initialize
	initCtx, cancelInit := context.WithTimeout(sigCtx, 9*time.Second)
	defer cancelInit()

	if err := e.handler.ColdStart(initCtx); err != nil {
		e.emitInitError(api, err)
		return err
	}

	for {
		select {
		case <-sigCtx.Done():
			// Must live inside the loop so we can react to termination BETWEEN invocations.
			// Moving this outside would miss signals that arrive after startup and before/after Next().
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := e.handler.Shutdown(shutdownCtx); err != nil {
				e.logger.Error(context.Background(), "shutdown error", "error", err)
			}
			cancel()
			return nil
		default:
		}

		// Per-invocation: Next() blocks until a unique event arrives.
		// This cannot be performed outside the loop because each iteration
		// corresponds to exactly one new invocation from the Runtime API.
		pollStart := time.Now()
		inv, nextErr := api.Next(sigCtx)
		if nextErr != nil {
			// Backoff must be applied per failure; doing this outside would either
			// block the entire loop or backoff when not needed.
			time.Sleep(100 * time.Millisecond)
			continue
		}
		nextMs := time.Since(pollStart).Milliseconds()

		// Per-invocation: The deadline and metadata vary for every invocation.
		// Contexts are immutable and must be derived per call to carry the current deadline.
		var (
			invokeCtx context.Context
			cancel    context.CancelFunc
		)
		// Reuse time buffer to avoid allocation if deadline is not critical precision.
		// This still must happen per-invocation since the deadline changes each time.
		if !inv.Deadline.IsZero() {
			e.timeBuffer = inv.Deadline
			invokeCtx, cancel = context.WithDeadline(sigCtx, e.timeBuffer)
		} else {
			invokeCtx, cancel = context.WithCancel(sigCtx)
		}
		// Get a cleaned RequestContext from the pool with environment pre-populated.
		rc := GetPooledRequestContext()
		// Per-invocation: Update request context with the current invocation metadata.
		// These values differ for every request and cannot be computed once.
		rc.AwsRequestID = inv.RequestID
		rc.InvokedFunctionArn = inv.InvokedFunctionArn
		rc.Deadline = inv.Deadline
		rc.TraceID = inv.TraceID
		// Per-invocation: Attach the RequestContext to the current invocation context.
		invokeCtx = NewContext(invokeCtx, rc)

		// Per-invocation: Reset reusable error struct to avoid stale error data.
		e.errorResponse.ErrorMessage = ""
		e.errorResponse.ErrorType = ""

		// Per-invocation: Reset event buffer and unmarshal the new payload.
		// Payloads differ per request and cannot be processed ahead of time.
		e.eventBuffer = *new(T) // Reset to zero value
		var (
			unmarshalMs int64
			validateMs  int64
			handlerMs   int64
			marshalMs   int64
			postMs      int64
		)
		if len(inv.Payload) > 0 {
			tUnmarshal := time.Now()
			if err := e.unmarshalWithPool(inv.Payload, &e.eventBuffer); err != nil {
				// Per-invocation: Error handling depends on the specific event and must
				// be emitted for this invocation only.
				e.errorResponse.ErrorMessage = err.Error()
				e.errorResponse.ErrorType = "UnmarshalError"
				tMarshal := time.Now()
				body, _ := e.marshalWithPool(e.errorResponse)
				marshalMs = time.Since(tMarshal).Milliseconds()
				tPost := time.Now()
				_ = api.Error(invokeCtx, inv.RequestID, body)
				postMs = time.Since(tPost).Milliseconds()
				unmarshalMs = time.Since(tUnmarshal).Milliseconds()
				// Emit structured metrics for benchmarking and bottleneck analysis
				e.logger.Info(invokeCtx, "invocation.metrics",
					"request_id", inv.RequestID,
					"outcome", "error",
					"error_type", "UnmarshalError",
					"next_ms", nextMs,
					"unmarshal_ms", unmarshalMs,
					"validate_ms", validateMs,
					"handler_ms", handlerMs,
					"marshal_ms", marshalMs,
					"post_ms", postMs,
					"total_ms", nextMs+unmarshalMs+validateMs+handlerMs+marshalMs+postMs,
				)
				if cancel != nil {
					cancel()
				}
				ReturnPooledRequestContext(rc)
				continue
			}
			unmarshalMs = time.Since(tUnmarshal).Milliseconds()
		}

		// Per-invocation: Validation is event-specific; cannot be hoisted.
		tValidate := time.Now()
		if err := e.handler.Validate(invokeCtx, e.eventBuffer); err != nil {
			e.errorResponse.ErrorMessage = err.Error()
			e.errorResponse.ErrorType = "ValidationError"
			tMarshal := time.Now()
			body, _ := e.marshalWithPool(e.errorResponse)
			marshalMs = time.Since(tMarshal).Milliseconds()
			tPost := time.Now()
			_ = api.Error(invokeCtx, inv.RequestID, body)
			postMs = time.Since(tPost).Milliseconds()
			validateMs = time.Since(tValidate).Milliseconds()
			e.logger.Info(invokeCtx, "invocation.metrics",
				"request_id", inv.RequestID,
				"outcome", "error",
				"error_type", "ValidationError",
				"next_ms", nextMs,
				"unmarshal_ms", unmarshalMs,
				"validate_ms", validateMs,
				"handler_ms", handlerMs,
				"marshal_ms", marshalMs,
				"post_ms", postMs,
				"total_ms", nextMs+unmarshalMs+validateMs+handlerMs+marshalMs+postMs,
			)
			if cancel != nil {
				cancel()
			}
			ReturnPooledRequestContext(rc)
			continue
		}
		validateMs = time.Since(tValidate).Milliseconds()

		// Per-invocation: Execute the handler with the current event and context.
		tHandler := time.Now()
		result, err := e.handler.Handler(invokeCtx, e.eventBuffer)
		handlerMs = time.Since(tHandler).Milliseconds()

		if err != nil {
			// Per-invocation: Map the error to a response for this specific request.
			e.errorResponse.ErrorMessage = err.Error()
			e.errorResponse.ErrorType = fmt.Sprintf("%T", err)
			tMarshal := time.Now()
			body, _ := e.marshalWithPool(e.errorResponse)
			marshalMs = time.Since(tMarshal).Milliseconds()

			tPost := time.Now()
			_ = api.Error(invokeCtx, inv.RequestID, body)
			postMs = time.Since(tPost).Milliseconds()
			e.logger.Info(invokeCtx, "invocation.metrics",
				"request_id", inv.RequestID,
				"outcome", "error",
				"error_type", fmt.Sprintf("%T", err),
				"next_ms", nextMs,
				"unmarshal_ms", unmarshalMs,
				"validate_ms", validateMs,
				"handler_ms", handlerMs,
				"marshal_ms", marshalMs,
				"post_ms", postMs,
				"total_ms", nextMs+unmarshalMs+validateMs+handlerMs+marshalMs+postMs,
			)
			if cancel != nil {
				cancel()
			}
			ReturnPooledRequestContext(rc)
		} else {
			// Per-invocation: Marshal the handler result for this specific request.
			tMarshal := time.Now()
			respBody, mErr := e.marshalWithPool(result)
			marshalMs = time.Since(tMarshal).Milliseconds()

			if mErr != nil {
				e.errorResponse.ErrorMessage = mErr.Error()
				e.errorResponse.ErrorType = "MarshalError"
				tPost := time.Now()
				body, _ := e.marshalWithPool(e.errorResponse)
				_ = api.Error(invokeCtx, inv.RequestID, body)
				postMs = time.Since(tPost).Milliseconds()
				e.logger.Info(invokeCtx, "invocation.metrics",
					"request_id", inv.RequestID,
					"outcome", "error",
					"error_type", "MarshalError",
					"next_ms", nextMs,
					"unmarshal_ms", unmarshalMs,
					"validate_ms", validateMs,
					"handler_ms", handlerMs,
					"marshal_ms", marshalMs,
					"post_ms", postMs,
					"total_ms", nextMs+unmarshalMs+validateMs+handlerMs+marshalMs+postMs,
				)
				if cancel != nil {
					cancel()
				}
				ReturnPooledRequestContext(rc)
				continue
			}

			// Per-invocation: POST the response for this exact invocation only.
			tPost := time.Now()
			_ = api.Response(invokeCtx, inv.RequestID, respBody)
			postMs = time.Since(tPost).Milliseconds()
			e.logger.Info(invokeCtx, "invocation.metrics",
				"request_id", inv.RequestID,
				"outcome", "success",
				"next_ms", nextMs,
				"unmarshal_ms", unmarshalMs,
				"validate_ms", validateMs,
				"handler_ms", handlerMs,
				"marshal_ms", marshalMs,
				"post_ms", postMs,
				"total_ms", nextMs+unmarshalMs+validateMs+handlerMs+marshalMs+postMs,
			)
			if cancel != nil {
				cancel()
			}
			ReturnPooledRequestContext(rc)
		}
	}
}

func (e *EventLoop[T, R]) emitInitError(api *runtimeapi.Client, err error) {
	// Phase 1 + 2 optimization: Reuse error response for init errors with optimized JSON
	e.errorResponse.ErrorMessage = err.Error()
	e.errorResponse.ErrorType = "InitError"
	body, _ := e.marshalWithPool(e.errorResponse)
	_ = api.InitError(context.Background(), body)
}

// Start is the entrypoint for running a Lambda handler.
// Example usage from main:
//
//	runtime.Start(NewHandler())
func Start[T, R any](h Handler[T, R]) {
	loop := NewEventLoop(h)
	if err := loop.Run(context.Background()); err != nil {
		// If the event loop returns an error, exit non-zero to signal failure.
		loop.logger.Error(context.Background(), "event loop error", "error", err.Error())
		os.Exit(1)
	}
}
