package runtime

import (
	"context"
	"os"
	"testing"
)

func TestContextMetadataFromEnvironment(t *testing.T) {
	// Test behavior: Lambda metadata should be available from RequestContext after env vars are set
	// This tests the roundtrip: env vars -> RequestContext -> accessible in context

	defer func() {
		os.Unsetenv("AWS_LAMBDA_LOG_GROUP_NAME")
		os.Unsetenv("AWS_LAMBDA_FUNCTION_NAME")
		os.Unsetenv("AWS_REGION")
	}()

	// Set environment variables
	os.Setenv("AWS_LAMBDA_LOG_GROUP_NAME", "test-log-group")
	os.Setenv("AWS_LAMBDA_FUNCTION_NAME", "test-function")
	os.Setenv("AWS_REGION", "us-east-1")

	// Create and populate RequestContext from environment
	rc := &RequestContext{}
	rc.PopulateFromEnvironment()

	// Test that behavior works correctly - can access Lambda metadata
	if rc.LogGroupName != "test-log-group" {
		t.Errorf("Lambda metadata not accessible: expected log group 'test-log-group', got '%s'", rc.LogGroupName)
	}
	if rc.FunctionName != "test-function" {
		t.Errorf("Lambda metadata not accessible: expected function name 'test-function', got '%s'", rc.FunctionName)
	}
	if rc.AWSRegion != "us-east-1" {
		t.Errorf("Lambda metadata not accessible: expected region 'us-east-1', got '%s'", rc.AWSRegion)
	}
}

func TestRequestContextRoundtrip(t *testing.T) {
	// Test behavior: Can store and retrieve Lambda request metadata through context
	original := &RequestContext{
		AwsRequestID: "test-request-123",
		TraceID:      "trace-456",
	}

	// Roundtrip: original -> context -> retrieved
	ctx := NewContext(context.Background(), original)
	retrieved, ok := FromContext(ctx)

	// Verify the roundtrip worked correctly
	if !ok {
		t.Fatal("Request context should be retrievable after storing")
	}
	if retrieved.AwsRequestID != original.AwsRequestID {
		t.Errorf("Request ID not preserved: expected '%s', got '%s'", original.AwsRequestID, retrieved.AwsRequestID)
	}
	if retrieved.TraceID != original.TraceID {
		t.Errorf("Trace ID not preserved: expected '%s', got '%s'", original.TraceID, retrieved.TraceID)
	}
}

func TestFromContextWithoutRequestContext(t *testing.T) {
	// Test behavior: Should safely handle context without Lambda metadata
	_, ok := FromContext(context.Background())
	if ok {
		t.Error("Should indicate no request context available when none stored")
	}
}

func TestWithRequestContextCreatesCopy(t *testing.T) {
	// Test behavior: WithRequestContext should safely copy data to prevent mutations
	original := RequestContext{AwsRequestID: "original-123"}

	ctx := WithRequestContext(context.Background(), original)

	// Modify original to verify it's a copy
	original.AwsRequestID = "modified-456"

	retrieved, ok := FromContext(ctx)
	if !ok {
		t.Fatal("Request context should be available")
	}
	if retrieved.AwsRequestID != "original-123" {
		t.Errorf("Context should contain original value, not mutation: got '%s'", retrieved.AwsRequestID)
	}
}

func TestContextOverwriteBehavior(t *testing.T) {
	// Test behavior: Later context values should override earlier ones
	ctx1 := NewContext(context.Background(), &RequestContext{AwsRequestID: "first"})
	ctx2 := NewContext(ctx1, &RequestContext{AwsRequestID: "second"})

	retrieved, ok := FromContext(ctx2)
	if !ok {
		t.Fatal("Should have request context available")
	}
	if retrieved.AwsRequestID != "second" {
		t.Errorf("Later context should override earlier: expected 'second', got '%s'", retrieved.AwsRequestID)
	}
}
