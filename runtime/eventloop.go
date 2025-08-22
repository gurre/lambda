package runtime

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/gurre/lambda/log"
	"github.com/gurre/lambda/runtimeapi"
)

var (
	// binaryStartTime tracks when the binary started for coldstart metrics
	binaryStartTime time.Time
)

func init() {
	binaryStartTime = time.Now()
	// Set GOMAXPROCS to NumCPU to prevent CPU contention on single vCPU environments
	// This ensures prefetch goroutines don't starve response marshalling
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

	// Prefetch state to avoid connection contention
	pendingPrefetch      chan *runtimeapi.Invocation
	pendingPrefetchError chan error
}

// TimingMetrics tracks detailed timing for ELTT attribution
// type TimingMetrics struct {
// 	InitStart       time.Time
// 	InitEnd         time.Time
// 	InvocationStart time.Time
// 	HandlerStart    time.Time
// 	HandlerEnd      time.Time
// 	MarshalStart    time.Time
// 	MarshalEnd      time.Time
// 	PostStart       time.Time
// 	PostEnd         time.Time
// 	PrefetchStart   time.Time
// 	PrefetchReady   time.Time
// }

func NewEventLoop[T, R any](h Handler[T, R]) *EventLoop[T, R] {
	logger := log.New(log.LevelInfo, os.Stdout)

	// Initialize RequestContext with environment metadata
	requestContext := RequestContext{}
	requestContext.PopulateFromEnvironment()

	e := &EventLoop[T, R]{
		handler: h,
		logger:  logger,
		// Initialize reusable objects with environment data pre-populated
		requestContext: requestContext,
		errorResponse:  ErrorResponse{},
		eventBuffer:    *new(T), // Zero value of type T
		// Initialize prefetch channels
		pendingPrefetch:      make(chan *runtimeapi.Invocation, 1),
		pendingPrefetchError: make(chan error, 1),
	}
	return e
}

// Optimized JSON marshaling with buffer reuse and Content-Length optimization
func (e *EventLoop[T, R]) marshalWithPool(v interface{}) ([]byte, error) {
	return log.JsoniterAPI.Marshal(v)
}

func (e *EventLoop[T, R]) Run(ctx context.Context) error {
	api, err := runtimeapi.NewClient()
	if err != nil {
		return err
	}

	// Initialize
	initCtx, cancelInit := context.WithTimeout(ctx, 9*time.Second)
	defer cancelInit()

	// Measure coldstart duration from binary start
	if err := e.handler.ColdStart(initCtx); err != nil {
		e.emitInitError(api, err)
		return err
	}

	// Initialize timing metrics for detailed ELTT attribution
	// timing := TimingMetrics{
	// 	InitStart: binaryStartTime,
	// 	InitEnd:   time.Now(),
	// }

	// Handle shutdown signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-sigCh:
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if err := e.handler.Shutdown(shutdownCtx); err != nil {
				e.logger.Error(context.Background(), "shutdown error", "error", err)
			}
			cancel()
			return nil
		default:
		}

		// Try to get invocation from prefetch first, then fall back to regular Next()
		var inv *runtimeapi.Invocation
		select {
		case inv = <-e.pendingPrefetch:
			// Got prefetched invocation - proceed immediately
		case prefetchErr := <-e.pendingPrefetchError:
			// Prefetch failed, fall back to regular Next()
			e.logger.Info(context.Background(), "prefetch.fallback", "error", prefetchErr.Error())
			var nextErr error
			inv, nextErr = api.Next()
			if nextErr != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
		default:
			// No prefetch available, use regular Next()
			var nextErr error
			inv, nextErr = api.Next()
			if nextErr != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		// timing.InvocationStart = time.Now()

		// Build context with deadline and metadata
		invokeCtx := ctx
		var cancel context.CancelFunc
		if !inv.Deadline.IsZero() {
			invokeCtx, cancel = context.WithDeadline(invokeCtx, inv.Deadline)
		}
		// Update per-invocation fields while preserving environment metadata
		e.requestContext.AwsRequestID = inv.RequestID
		e.requestContext.InvokedFunctionArn = inv.InvokedFunctionArn
		e.requestContext.Deadline = inv.Deadline
		e.requestContext.TraceID = inv.TraceID
		// Clear per-invocation fields to avoid stale data
		e.requestContext.Identity = CognitoIdentity{}
		e.requestContext.ClientContext = ClientContext{}
		// Note: Environment metadata (AWSRegion, FunctionName, etc.) remains populated

		invokeCtx = NewContext(invokeCtx, &e.requestContext)

		// Optimization: Reuse error response struct (clear previous values)
		e.errorResponse.ErrorMessage = ""
		e.errorResponse.ErrorType = ""

		// Optimization: Reuse event buffer with optimized JSON
		e.eventBuffer = *new(T) // Reset to zero value
		if len(inv.Payload) > 0 {
			if err := log.JsoniterAPI.Unmarshal(inv.Payload, &e.eventBuffer); err != nil {
				e.errorResponse.ErrorMessage = err.Error()
				e.errorResponse.ErrorType = "UnmarshalError"
				body, _ := e.marshalWithPool(e.errorResponse)
				_ = api.Error(inv.RequestID, body)
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
			_ = api.Error(inv.RequestID, body)
			if cancel != nil {
				cancel()
			}
			continue
		}

		// Process invocation
		// timing.HandlerStart = time.Now()
		result, err := e.handler.Handler(invokeCtx, e.eventBuffer)
		// timing.HandlerEnd = time.Now()

		if err != nil {
			// Handle errors with proper timing
			// timing.MarshalStart = time.Now()
			e.errorResponse.ErrorMessage = err.Error()
			e.errorResponse.ErrorType = fmt.Sprintf("%T", err)
			body, _ := e.marshalWithPool(e.errorResponse)
			// timing.MarshalEnd = time.Now()

			// timing.PostStart = time.Now()
			_ = api.Error(inv.RequestID, body)
			// timing.PostEnd = time.Now()
			if cancel != nil {
				cancel()
			}
		} else {
			// Marshal response completely FIRST - critical for avoiding contention
			// timing.MarshalStart = time.Now()
			respBody, mErr := e.marshalWithPool(result)
			// timing.MarshalEnd = time.Now()

			if mErr != nil {
				e.errorResponse.ErrorMessage = mErr.Error()
				e.errorResponse.ErrorType = "MarshalError"
				body, _ := e.marshalWithPool(e.errorResponse)
				// timing.PostStart = time.Now()
				_ = api.Error(inv.RequestID, body)
				// timing.PostEnd = time.Now()
				if cancel != nil {
					cancel()
				}
				continue
			}

			// Log timing metrics AFTER posting response to avoid blocking the runtime
			// This is a waste of time to log here.
			// go e.logDetailedTiming(invokeCtx, timing)

			// Immediately POST the result on separate connection
			// timing.PostStart = time.Now()
			_ = api.Response(inv.RequestID, respBody)
			// timing.PostEnd = time.Now()

			// Start prefetch ONLY after POST is complete to prevent connection contention
			// timing.PrefetchStart = time.Now()
			e.startPrefetch(api)
			// timing.PrefetchReady = time.Now()
			if cancel != nil {
				cancel()
			}
		}

	}
}

// startPrefetch safely starts a prefetch operation without blocking the event loop
func (e *EventLoop[T, R]) startPrefetch(api *runtimeapi.Client) {
	// Clear any previous prefetch results
	select {
	case <-e.pendingPrefetch:
	default:
	}
	select {
	case <-e.pendingPrefetchError:
	default:
	}

	// Start new prefetch in goroutine
	go func() {
		nextCh, errCh := api.PrefetchNext()
		select {
		case inv := <-nextCh:
			// Try to send prefetched invocation, don't block if buffer is full
			select {
			case e.pendingPrefetch <- inv:
			default:
				// Channel full, discard this prefetch (shouldn't happen with buffer size 1)
			}
		case err := <-errCh:
			// Try to send error, don't block if buffer is full
			select {
			case e.pendingPrefetchError <- err:
			default:
				// Channel full, discard this error (shouldn't happen with buffer size 1)
			}
		}
	}()
}

func (e *EventLoop[T, R]) emitInitError(api *runtimeapi.Client, err error) {
	// Phase 1 + 2 optimization: Reuse error response for init errors with optimized JSON
	e.errorResponse.ErrorMessage = err.Error()
	e.errorResponse.ErrorType = "InitError"
	body, _ := e.marshalWithPool(e.errorResponse)
	_ = api.InitError(body)
}

// logDetailedTiming logs detailed ELTT attribution metrics for debugging tail tax
// func (e *EventLoop[T, R]) logDetailedTiming(ctx context.Context, timing TimingMetrics) {
// 	if timing.HandlerStart.IsZero() || timing.HandlerEnd.IsZero() {
// 		return // Skip if handler didn't run
// 	}

// 	// Use microseconds for better precision, then convert to fractional milliseconds
// 	handlerExecUs := timing.HandlerEnd.Sub(timing.HandlerStart).Microseconds()
// 	handlerExecMs := float64(handlerExecUs) / 1000.0

// 	var marshalMs, postMs, prefetchBlockMs float64

// 	if !timing.MarshalStart.IsZero() && !timing.MarshalEnd.IsZero() {
// 		marshalUs := timing.MarshalEnd.Sub(timing.MarshalStart).Microseconds()
// 		marshalMs = float64(marshalUs) / 1000.0
// 	}

// 	if !timing.PostStart.IsZero() && !timing.PostEnd.IsZero() {
// 		postUs := timing.PostEnd.Sub(timing.PostStart).Microseconds()
// 		postMs = float64(postUs) / 1000.0
// 	}

// 	if !timing.PrefetchReady.IsZero() && !timing.PostEnd.IsZero() {
// 		prefetchBlockUs := timing.PrefetchReady.Sub(timing.PostEnd).Microseconds()
// 		prefetchBlockMs = float64(prefetchBlockUs) / 1000.0
// 	}

// 	// Calculate total time since invocation start
// 	totalUs := time.Since(timing.InvocationStart).Microseconds()
// 	totalMs := float64(totalUs) / 1000.0

// 	// ELTT = total - handler execution time
// 	elttMs := totalMs - handlerExecMs

// 	// Estimated rounding tax (Lambda rounds to nearest 1ms)
// 	roundingMs := math.Ceil(handlerExecMs) - handlerExecMs

// 	e.logger.Info(ctx, "invocation.timing",
// 		"handlerExecMs", handlerExecMs,
// 		"marshalMs", marshalMs,
// 		"postMs", postMs,
// 		"prefetchBlockMs", prefetchBlockMs,
// 		"elttMs", elttMs,
// 		"totalMs", totalMs,
// 		"roundingTaxMs", roundingMs,
// 	)
// }

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
