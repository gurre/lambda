package runtime

import (
	"context"
)

// Handler defines a lifecycle-aware Lambda handler for specific input type T and output type R.
// Validate and Process both receive the same event type T, and Process returns type R.
type Handler[T, R any] interface {
	ColdStart(ctx context.Context) error
	Validate(ctx context.Context, event T) error
	Handler(ctx context.Context, event T) (R, error)
	Shutdown(ctx context.Context) error
}
