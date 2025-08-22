package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/gurre/lambda/log"
)

// DefaultLifecycle provides default lifecycle methods with logger, env, and lambda context access.
type DefaultLifecycle[T, R any] struct {
	Logger log.Logger
}

// NewDefaultLifecycle creates a new DefaultLifecycle with the provided logger and env.
func NewDefaultLifecycle[T, R any](logger log.Logger) *DefaultLifecycle[T, R] {
	return &DefaultLifecycle[T, R]{
		Logger: logger,
	}
}

func (d *DefaultLifecycle[T, R]) ColdStart(ctx context.Context) error {
	return nil
}

func (d *DefaultLifecycle[T, R]) Validate(ctx context.Context, event T) error { return nil }

func (d *DefaultLifecycle[T, R]) Handler(ctx context.Context, event T) (R, error) {
	return *new(R), nil
}

func (d *DefaultLifecycle[T, R]) Shutdown(ctx context.Context) error { return nil }

var _ Handler[any, any] = &DefaultLifecycle[any, any]{}

// Mock logger for testing
type mockLogger struct {
	logs []logEntry
}

type logEntry struct {
	level   string
	message string
	args    []any
}

func (m *mockLogger) With(args ...any) log.Logger {
	return m // Simplified for testing
}

func (m *mockLogger) WithError(err error) log.Logger {
	return m // Simplified for testing
}

func (m *mockLogger) Debug(ctx context.Context, msg string, args ...any) {
	m.logs = append(m.logs, logEntry{level: "debug", message: msg, args: args})
}

func (m *mockLogger) Info(ctx context.Context, msg string, args ...any) {
	m.logs = append(m.logs, logEntry{level: "info", message: msg, args: args})
}

func (m *mockLogger) Warn(ctx context.Context, msg string, args ...any) {
	m.logs = append(m.logs, logEntry{level: "warn", message: msg, args: args})
}

func (m *mockLogger) Error(ctx context.Context, msg string, args ...any) {
	m.logs = append(m.logs, logEntry{level: "error", message: msg, args: args})
}

// Test event and response types
type TestEvent struct {
	Name string `json:"name"`
	ID   int    `json:"id"`
}

type TestResponse struct {
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Test handler implementation
type testHandler struct {
	*DefaultLifecycle[TestEvent, TestResponse]
	initCalled     bool
	shutdownCalled bool
	validateError  error
	handlerError   error
}

func newTestHandler() *testHandler {
	return &testHandler{
		DefaultLifecycle: &DefaultLifecycle[TestEvent, TestResponse]{},
	}
}

func (h *testHandler) ColdStart(ctx context.Context) error {
	h.initCalled = true
	return h.DefaultLifecycle.ColdStart(ctx)
}

func (h *testHandler) Validate(ctx context.Context, event TestEvent) error {
	if h.validateError != nil {
		return h.validateError
	}
	return h.DefaultLifecycle.Validate(ctx, event)
}

func (h *testHandler) Handler(ctx context.Context, event TestEvent) (TestResponse, error) {
	if h.handlerError != nil {
		return TestResponse{}, h.handlerError
	}

	return TestResponse{
		Message: "Hello " + event.Name,
		Status:  "success",
	}, nil
}

func (h *testHandler) Shutdown(ctx context.Context) error {
	h.shutdownCalled = true
	return h.DefaultLifecycle.Shutdown(ctx)
}

func TestHandlerCompleteLifecycle(t *testing.T) {
	// Test behavior: Handler should work through complete lifecycle without errors
	handler := newTestHandler()
	event := TestEvent{Name: "test", ID: 1}
	ctx := context.Background()

	// Complete lifecycle should work without errors
	if err := handler.ColdStart(ctx); err != nil {
		t.Fatalf("Handler lifecycle should start successfully: %v", err)
	}

	if err := handler.Validate(ctx, event); err != nil {
		t.Fatalf("Handler should validate valid events: %v", err)
	}

	response, err := handler.Handler(ctx, event)
	if err != nil {
		t.Fatalf("Handler should process valid events: %v", err)
	}

	// Test that processing actually works
	if response.Message != "Hello test" {
		t.Errorf("Handler should process events correctly: expected 'Hello test', got '%s'", response.Message)
	}

	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("Handler lifecycle should shutdown successfully: %v", err)
	}
}

func TestHandlerErrorHandling(t *testing.T) {
	// Test behavior: Handler should properly propagate validation and processing errors

	// Validation error case
	handler := newTestHandler()
	expectedValidationError := errors.New("validation failed")
	handler.validateError = expectedValidationError

	event := TestEvent{Name: "invalid", ID: -1}
	if err := handler.Validate(context.Background(), event); err != expectedValidationError {
		t.Errorf("Handler should propagate validation errors: expected %v, got %v", expectedValidationError, err)
	}

	// Processing error case
	handler2 := newTestHandler()
	expectedProcessingError := errors.New("processing failed")
	handler2.handlerError = expectedProcessingError

	_, err := handler2.Handler(context.Background(), TestEvent{Name: "test", ID: 1})
	if err != expectedProcessingError {
		t.Errorf("Handler should propagate processing errors: expected %v, got %v", expectedProcessingError, err)
	}
}
