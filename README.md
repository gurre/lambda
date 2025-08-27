## Performant Lambda Custom Runtime for Go

A custom runtime for AWS Lambda with a typed handler interface, efficient JSON handling, an event loop tuned for low tail latency, and a structured logger.

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
