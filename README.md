## Performant Lambda Custom Runtime for Go

A custom runtime for AWS Lambda with a typed handler interface, efficient JSON handling, an event loop tuned for low tail latency, and a structured, zero-drama logger.

- Fast event loop: early /invocation/next prefetch without head-of-line blocking.
- Lean networking: one shared http.Transport, persistent connections, no proxies, no gzip, HTTP/1.1 only.
- Stable latency: no chunked bodies, bodies fully drained for reuse, minimal header churn.
- Typed handlers: zero reflection on the hot path.
- Purpose-built logger: structured JSON, low allocation, per-invocation fields.

AWS bills the wall-clock time between when your handler starts and when your runtime posts the result. The runtime itself can add avoidable milliseconds: connection stalls, unnecessary allocations, chunked encoding, logging on the hot path, or a poorly timed /invocation/next.

### Example

```go
type Input struct { Input string `json:"input"` }
type Output struct { Message string `json:"message"` }

type Handler struct{}

func (h *Handler) ColdStart(ctx context.Context) error { return nil }
func (h *Handler) Validate(ctx context.Context, in Input) error {
    if in.Input == "" {
        return errors.New("input required")
    };
    return nil
}
func (h *Handler) Handler(ctx context.Context, in Input) (Output, error) {
    return Output{Message: in.Input}, nil
}
func (h *Handler) Shutdown(ctx context.Context) error { return nil }

func main() { runtime.Start(&Handler{}) }
```
