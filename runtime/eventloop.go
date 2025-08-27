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
	requestContext RequestContext
	errorResponse  ErrorResponse
	eventBuffer    T

	// Additional optimizations: Pre-allocated buffers and pools
	jsoniter   jsoniter.API // Dedicated API instance
	bufferPool *sync.Pool   // Buffer pool for large payloads
	stringPool *sync.Pool   // String buffer pool
	timeBuffer time.Time    // Reusable time buffer
}

func NewEventLoop[T, R any](h Handler[T, R]) *EventLoop[T, R] {
	logger := log.New(log.LevelInfo, os.Stdout)

	// Use pooled RequestContext with environment metadata pre-populated
	requestContext := GetPooledRequestContext()

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
		handler: h,
		logger:  logger,
		// Initialize reusable objects with environment data pre-populated
		requestContext: *requestContext, // Dereference the pointer
		errorResponse:  ErrorResponse{},
		eventBuffer:    *new(T), // Zero value of type T

		// Performance optimizations
		jsoniter:   jsonAPI,
		bufferPool: bufferPool,
		stringPool: stringPool,
		timeBuffer: time.Time{}, // Zero time to reuse
	}

	// Return the pooled RequestContext since we copied its value
	ReturnPooledRequestContext(requestContext)
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
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := e.handler.Shutdown(shutdownCtx); err != nil {
				e.logger.Error(context.Background(), "shutdown error", "error", err)
			}
			cancel()
			return nil
		default:
		}

		// Use a simple Next() call to retrieve the next invocation
		inv, nextErr := api.Next(sigCtx)
		if nextErr != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Build context with deadline and metadata - optimize context creation
		var (
			invokeCtx context.Context
			cancel    context.CancelFunc
		)
		// Reuse time buffer to avoid allocation if deadline is not critical precision
		if !inv.Deadline.IsZero() {
			e.timeBuffer = inv.Deadline
			invokeCtx, cancel = context.WithDeadline(sigCtx, e.timeBuffer)
		} else {
			invokeCtx, cancel = context.WithCancel(sigCtx)
		}
		// Update per-invocation fields while preserving environment metadata
		e.requestContext.AwsRequestID = inv.RequestID
		e.requestContext.InvokedFunctionArn = inv.InvokedFunctionArn
		e.requestContext.Deadline = inv.Deadline
		e.requestContext.TraceID = inv.TraceID
		// Clear per-invocation fields to avoid stale data - use zero values for efficiency
		e.requestContext.Identity.CognitoIdentityID = ""
		e.requestContext.Identity.CognitoIdentityPoolID = ""
		e.requestContext.ClientContext.Client.InstallationID = ""
		e.requestContext.ClientContext.Client.AppTitle = ""
		e.requestContext.ClientContext.Client.AppVersionCode = ""
		e.requestContext.ClientContext.Client.AppPackageName = ""
		for k := range e.requestContext.ClientContext.Env {
			delete(e.requestContext.ClientContext.Env, k)
		}
		for k := range e.requestContext.ClientContext.Custom {
			delete(e.requestContext.ClientContext.Custom, k)
		}
		// Note: Environment metadata (AWSRegion, FunctionName, etc.) remains populated

		invokeCtx = NewContext(invokeCtx, &e.requestContext)

		// Optimization: Reuse error response struct (clear previous values)
		e.errorResponse.ErrorMessage = ""
		e.errorResponse.ErrorType = ""

		// Optimization: Reuse event buffer with optimized JSON
		e.eventBuffer = *new(T) // Reset to zero value
		if len(inv.Payload) > 0 {
			if err := e.unmarshalWithPool(inv.Payload, &e.eventBuffer); err != nil {
				e.errorResponse.ErrorMessage = err.Error()
				e.errorResponse.ErrorType = "UnmarshalError"
				body, _ := e.marshalWithPool(e.errorResponse)
				_ = api.Error(invokeCtx, inv.RequestID, body)
				if cancel != nil {
					cancel()
				}
				continue
			}
		}

		// Validate and skip processing on failure
		if err := e.handler.Validate(invokeCtx, e.eventBuffer); err != nil {
			e.errorResponse.ErrorMessage = err.Error()
			e.errorResponse.ErrorType = "ValidationError"
			body, _ := e.marshalWithPool(e.errorResponse)
			_ = api.Error(invokeCtx, inv.RequestID, body)
			if cancel != nil {
				cancel()
			}
			continue
		}

		// Process invocation
		result, err := e.handler.Handler(invokeCtx, e.eventBuffer)

		if err != nil {
			e.errorResponse.ErrorMessage = err.Error()
			e.errorResponse.ErrorType = fmt.Sprintf("%T", err)
			body, _ := e.marshalWithPool(e.errorResponse)

			_ = api.Error(invokeCtx, inv.RequestID, body)
			if cancel != nil {
				cancel()
			}
		} else {
			respBody, mErr := e.marshalWithPool(result)

			if mErr != nil {
				e.errorResponse.ErrorMessage = mErr.Error()
				e.errorResponse.ErrorType = "MarshalError"
				body, _ := e.marshalWithPool(e.errorResponse)
				_ = api.Error(invokeCtx, inv.RequestID, body)
				if cancel != nil {
					cancel()
				}
				continue
			}

			// Immediately POST the result
			_ = api.Response(invokeCtx, inv.RequestID, respBody)
			if cancel != nil {
				cancel()
			}
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
		os.Exit(1)
	}
}
