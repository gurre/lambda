package runtime

import (
	"context"
	"fmt"
	"testing"
)

// Integration test types
type IntegrationEvent struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

type IntegrationResponse struct {
	Greeting  string `json:"greeting"`
	Processed bool   `json:"processed"`
}

// Integration test handler
type integrationHandler struct {
	initialized     bool
	shutdownCalled  bool
	processedEvents []IntegrationEvent
}

func (h *integrationHandler) ColdStart(ctx context.Context) error {
	h.initialized = true
	return nil
}

func (h *integrationHandler) Validate(ctx context.Context, event IntegrationEvent) error {
	if event.Name == "" {
		return fmt.Errorf("name is required")
	}
	return nil
}

func (h *integrationHandler) Handler(ctx context.Context, event IntegrationEvent) (IntegrationResponse, error) {
	h.processedEvents = append(h.processedEvents, event)
	return IntegrationResponse{
		Greeting:  "Hello, " + event.Name + "! Message: " + event.Message,
		Processed: true,
	}, nil
}

func (h *integrationHandler) Shutdown(ctx context.Context) error {
	h.shutdownCalled = true
	return nil
}

// TestCompleteHandlerLifecycle tests the complete handler lifecycle integration
func TestCompleteHandlerLifecycle(t *testing.T) {
	handler := &integrationHandler{}
	eventLoop := NewEventLoop[IntegrationEvent, IntegrationResponse](handler)

	if eventLoop == nil {
		t.Fatal("EventLoop should be created successfully")
	}

	ctx := context.Background()

	// Test ColdStart
	err := handler.ColdStart(ctx)
	if err != nil {
		t.Fatalf("ColdStart failed: %v", err)
	}
	if !handler.initialized {
		t.Error("Handler should be marked as initialized after ColdStart")
	}

	// Test event validation
	validEvent := IntegrationEvent{Name: "World", Message: "Hello"}
	err = handler.Validate(ctx, validEvent)
	if err != nil {
		t.Errorf("Valid event should pass validation: %v", err)
	}

	invalidEvent := IntegrationEvent{Name: "", Message: "Hello"}
	err = handler.Validate(ctx, invalidEvent)
	if err == nil {
		t.Error("Invalid event should fail validation")
	}

	// Test event processing
	response, err := handler.Handler(ctx, validEvent)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}

	expectedResponse := IntegrationResponse{
		Greeting:  "Hello, World! Message: Hello",
		Processed: true,
	}
	if response != expectedResponse {
		t.Errorf("Expected response %+v, got %+v", expectedResponse, response)
	}

	if len(handler.processedEvents) != 1 {
		t.Errorf("Expected 1 processed event, got %d", len(handler.processedEvents))
	}

	// Test shutdown
	err = handler.Shutdown(ctx)
	if err != nil {
		t.Errorf("Shutdown failed: %v", err)
	}
	if !handler.shutdownCalled {
		t.Error("Handler should be marked as shutdown after Shutdown")
	}
}

// TestEventLoopIntegration tests EventLoop integration with handlers
func TestEventLoopIntegration(t *testing.T) {
	handler := &integrationHandler{}
	eventLoop := NewEventLoop(handler)

	if eventLoop == nil {
		t.Fatal("Should be able to create EventLoop with handler")
	}

	// Test handler works through the EventLoop interface
	event := IntegrationEvent{Name: "Integration", Message: "Test"}
	response, err := handler.Handler(context.Background(), event)
	if err != nil {
		t.Fatalf("Handler should process events: %v", err)
	}
	if response.Greeting != "Hello, Integration! Message: Test" {
		t.Errorf("Handler should process correctly: expected greeting with 'Integration', got '%s'", response.Greeting)
	}
}
