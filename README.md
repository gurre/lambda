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

### Local benchmarking with AWS Lambda RIE

This repo includes a benchmark suite that runs the container with the AWS Lambda Runtime Interface Emulator (RIE), invokes the function via HTTP, parses emulator REPORT lines for billed time, and correlates them with internal per-phase timings emitted by the event loop.

Run:

```bash
go test -bench=BenchmarkRIEEndToEnd -run ^$ ./bench -benchmem
go test -bench=BenchmarkRIEHeavy -run ^$ ./bench -benchmem
```

The event loop logs structured metrics per invocation as JSON with message `invocation.metrics`:

```json
{"message":"invocation.metrics","request_id":"...","outcome":"success","next_ms":0,"unmarshal_ms":0,"validate_ms":0,"handler_ms":35,"marshal_ms":0,"post_ms":0,"total_ms":35}
```

### Findings (sample on Apple M4 Pro)

Benchmark: heavy synthetic workload using inputs `sleep:20ms`, `sleep:50ms`, `cpu:20ms`, `cpu:50ms`.

| Metric            | Average (ms) |
| ----------------- | -----------: |
| next_ms           |        ~0.04 |
| unmarshal_ms      |        ~0.00 |
| validate_ms       |        ~0.00 |
| handler_ms        |       ~35.23 |
| marshal_ms        |        ~0.00 |
| post_ms           |        ~0.00 |
| total_internal_ms |       ~35.27 |
| billed_ms (RIE)   |       ~36.57 |

Notes:
- Under heavier load, time is almost entirely spent in the handler (`handler_ms`), as expected. JSON decode/encode and event loop overheads remain near-zero relative to the workload.
- Billed duration (from RIE) is slightly higher than internal total due to rounding/overhead (typically ~1 ms).
- For micro workloads, billed time rounds up to 1 ms and will dominate; to surface event loop bottlenecks, use heavier payloads or add CPU/sleep in the handler as shown.

To reproduce locally with Podman:

```bash
podman build -t testlambda .
podman run -d -p 9000:8080 --entrypoint /usr/local/bin/aws-lambda-rie testlambda:latest ./bootstrap
curl "http://localhost:9000/2015-03-31/functions/function/invocations" -d '{"input":"cpu:50ms"}'
podman logs <container_id>
```
