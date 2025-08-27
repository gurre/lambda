package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gurre/lambda/log"
	jsoniter "github.com/json-iterator/go"
)

// Benchmark event types
type BenchEvent struct {
	ID       int                    `json:"id"`
	Message  string                 `json:"message"`
	Data     []byte                 `json:"data"`
	Metadata map[string]interface{} `json:"metadata"`
}

type BenchResponse struct {
	Status    string `json:"status"`
	Result    int    `json:"result"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
}

// Benchmark handler
type benchHandler struct{}

func (h *benchHandler) ColdStart(ctx context.Context) error {
	return nil
}

func (h *benchHandler) Validate(ctx context.Context, event BenchEvent) error {
	return nil
}

func (h *benchHandler) Handler(ctx context.Context, event BenchEvent) (BenchResponse, error) {
	return BenchResponse{
		Status:    "ok",
		Result:    event.ID * 2,
		Message:   "processed: " + event.Message,
		Timestamp: time.Now().Unix(),
	}, nil
}

func (h *benchHandler) Shutdown(ctx context.Context) error {
	return nil
}

// BenchmarkEventLoopCreation measures EventLoop creation performance
func BenchmarkEventLoopCreation(b *testing.B) {
	handler := &benchHandler{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewEventLoop[BenchEvent, BenchResponse](handler)
	}
}

// BenchmarkRequestContextPooling compares pooled vs non-pooled RequestContext
func BenchmarkRequestContextPooling(b *testing.B) {
	b.Run("Traditional-Allocation", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rc := &RequestContext{}
			rc.PopulateFromEnvironment()
			rc.AwsRequestID = "req-123"
			rc.TraceID = "trace-456"
			_ = rc
		}
	})

	b.Run("Pooled-RequestContext", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			rc := GetPooledRequestContext()
			rc.AwsRequestID = "req-123"
			rc.TraceID = "trace-456"
			ReturnPooledRequestContext(rc)
		}
	})
}

// BenchmarkJSONPerformance compares standard JSON vs jsoniter
func BenchmarkJSONPerformance(b *testing.B) {
	event := BenchEvent{
		ID:      123,
		Message: "benchmark event",
		Data:    make([]byte, 100),
		Metadata: map[string]interface{}{
			"version": "1.0",
			"source":  "test",
		},
	}

	response := BenchResponse{
		Status:    "success",
		Result:    246,
		Message:   "processed",
		Timestamp: time.Now().Unix(),
	}

	b.Run("StandardJSON-Marshal", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = json.Marshal(response)
		}
	})

	b.Run("JsonIter-Marshal", func(b *testing.B) {
		jsonAPI := jsoniter.Config{
			EscapeHTML:             false,
			SortMapKeys:            false,
			ValidateJsonRawMessage: false,
		}.Froze()

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = jsonAPI.Marshal(response)
		}
	})

	b.Run("StandardJSON-Unmarshal", func(b *testing.B) {
		data, _ := json.Marshal(event)
		var e BenchEvent

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			e = BenchEvent{}
			_ = json.Unmarshal(data, &e)
		}
	})

	b.Run("JsonIter-Unmarshal", func(b *testing.B) {
		jsonAPI := jsoniter.Config{
			EscapeHTML:             false,
			SortMapKeys:            false,
			ValidateJsonRawMessage: false,
		}.Froze()

		data, _ := jsonAPI.Marshal(event)
		var e BenchEvent

		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			e = BenchEvent{}
			_ = jsonAPI.Unmarshal(data, &e)
		}
	})
}

// BenchmarkBufferPooling tests sync.Pool effectiveness for JSON marshaling
func BenchmarkBufferPooling(b *testing.B) {
	handler := &benchHandler{}
	eventLoop := NewEventLoop[BenchEvent, BenchResponse](handler)

	response := BenchResponse{
		Status:    "success",
		Result:    246,
		Message:   "processed message",
		Timestamp: time.Now().Unix(),
	}

	b.Run("Direct-JsonIter", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = log.JsoniterAPI.Marshal(response)
		}
	})

	b.Run("Pool-Optimized", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = eventLoop.marshalWithPool(response)
		}
	})
}

// BenchmarkRequestContext measures RequestContext optimization
func BenchmarkRequestContext(b *testing.B) {
	// Removed unused handler variable

	b.Run("Original-Allocation", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx := context.Background()
			rc := RequestContext{
				AwsRequestID:       "req-123",
				InvokedFunctionArn: "arn:aws:lambda:us-east-1:123:function:test",
				TraceID:            "trace-456",
				Deadline:           time.Now().Add(5 * time.Minute),
			}
			_ = WithRequestContext(ctx, rc)
		}
	})

	b.Run("Optimized-Reuse", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx := context.Background()
			rc := GetPooledRequestContext()
			rc.AwsRequestID = "req-123"
			rc.InvokedFunctionArn = "arn:aws:lambda:us-east-1:123:function:test"
			rc.TraceID = "trace-456"
			rc.Deadline = time.Now().Add(5 * time.Minute)
			_ = NewContext(ctx, rc)
			ReturnPooledRequestContext(rc)
		}
	})
}

// BenchmarkOverallPerformance compares optimized vs unoptimized processing
func BenchmarkOverallPerformance(b *testing.B) {
	payload := []byte(`{
		"id": 42,
		"message": "benchmark event",
		"data": "dGVzdCBkYXRh",
		"metadata": {
			"version": "1.0",
			"source": "benchmark"
		}
	}`)

	handler := &benchHandler{}
	eventLoop := NewEventLoop[BenchEvent, BenchResponse](handler)

	b.Run("Unoptimized-Simulation", func(b *testing.B) {
		// Simulate pre-optimization approach
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx := context.Background()

			// Create new RequestContext each time
			rc := RequestContext{
				AwsRequestID: "req-123",
				TraceID:      "trace-456",
			}
			rc.PopulateFromEnvironment() // Expensive repeated env var access
			invokeCtx := WithRequestContext(ctx, rc)

			// Unmarshal with standard JSON
			var event BenchEvent
			_ = json.Unmarshal(payload, &event)

			// Process
			_ = handler.Validate(ctx, event)
			result, _ := handler.Handler(invokeCtx, event)

			// Marshal with standard JSON
			_, _ = json.Marshal(result)

			// Create error response struct each time
			errResp := struct {
				ErrorMessage string `json:"errorMessage"`
				ErrorType    string `json:"errorType"`
			}{}
			_ = errResp
		}
	})

	b.Run("Optimized-Implementation", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx := context.Background()

			// Reuse RequestContext with cached environment via pool
			rc := GetPooledRequestContext()
			rc.AwsRequestID = "req-123"
			rc.TraceID = "trace-456"
			invokeCtx := NewContext(ctx, rc)

			// Reuse event buffer with optimized JSON
			eventLoop.eventBuffer = BenchEvent{}
			_ = eventLoop.unmarshalWithPool(payload, &eventLoop.eventBuffer)

			// Process
			_ = handler.Validate(ctx, eventLoop.eventBuffer)
			result, _ := handler.Handler(invokeCtx, eventLoop.eventBuffer)

			// Marshal with buffer pooling and optimized JSON
			_, _ = eventLoop.marshalWithPool(result)
			ReturnPooledRequestContext(rc)

			// Error response is pre-allocated (no additional allocation)
		}
	})

	b.Run("Ultra-Optimized", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		// Pre-allocate and reuse everything possible
		rc := GetPooledRequestContext()
		defer ReturnPooledRequestContext(rc)

		for i := 0; i < b.N; i++ {
			ctx := context.Background()

			// Just update the request-specific fields
			rc.AwsRequestID = "req-123"
			rc.TraceID = "trace-456"
			invokeCtx := NewContext(ctx, rc)

			// Reuse event buffer
			eventLoop.eventBuffer = BenchEvent{}
			_ = eventLoop.unmarshalWithPool(payload, &eventLoop.eventBuffer)

			// Process
			_ = handler.Validate(ctx, eventLoop.eventBuffer)
			result, _ := handler.Handler(invokeCtx, eventLoop.eventBuffer)

			// Marshal with all optimizations
			_, _ = eventLoop.marshalWithPool(result)
		}
	})
}

// BenchmarkMemoryFootprint measures memory usage patterns
func BenchmarkMemoryFootprint(b *testing.B) {
	handler := &benchHandler{}
	eventLoop := NewEventLoop[BenchEvent, BenchResponse](handler)

	payload := []byte(`{"id": 42, "message": "test", "data": "dGVzdA==", "metadata": {}}`)

	b.Run("InvocationProcessing", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			ctx := context.Background()

			// Simulate complete invocation processing
			rc := GetPooledRequestContext()
			rc.AwsRequestID = "req-123"
			eventLoop.eventBuffer = BenchEvent{}
			_ = eventLoop.unmarshalWithPool(payload, &eventLoop.eventBuffer)

			result, _ := handler.Handler(ctx, eventLoop.eventBuffer)
			_, _ = eventLoop.marshalWithPool(result)
			ReturnPooledRequestContext(rc)
		}
	})
}
