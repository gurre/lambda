package runtimeapi

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

// Test behavior: parseDeadline should correctly convert Unix ms header to time.Time
func TestParseDeadline(t *testing.T) {
	// 1. Known millisecond timestamp
	ms := int64(1_700_000_000_000)
	h := http.Header{}
	h.Set(headerDeadlineMS, "1700000000000")

	got := parseDeadline(h)
	want := time.Unix(0, ms*int64(time.Millisecond))

	if !got.Equal(want) {
		t.Errorf("parseDeadline: expected %v, got %v", want, got)
	}

	// 2. Missing header should yield zero time
	h2 := http.Header{}
	got2 := parseDeadline(h2)
	if !got2.IsZero() {
		t.Errorf("parseDeadline: expected zero time for missing header, got %v", got2)
	}
}

// Test behavior: parseInvocation should return error for non-200 status
func TestParseInvocationErrorStatus(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Status:     "500 Internal Server Error",
		Body:       io.NopCloser(bytes.NewBufferString("boom")),
		Header:     http.Header{},
	}

	inv, err := parseInvocation(resp)
	if err == nil {
		t.Fatal("parseInvocation should return error for non-200 status")
	}
	if inv != nil {
		t.Errorf("parseInvocation should return nil invocation on error, got %+v", inv)
	}
}

// Test behavior: parseInvocation should extract headers and payload on success
func TestParseInvocationSuccess(t *testing.T) {
	payload := []byte(`{"hello":"world"}`)
	h := http.Header{}
	h.Set(headerAWSRequestID, "req-123")
	h.Set(headerInvokedFunctionARN, "arn:aws:lambda:us-east-1:123:function:test")
	h.Set(headerDeadlineMS, "1700000000000")
	h.Set(headerTraceID, "Root=1-abc;Parent=def;Sampled=1")
	h.Set(headerCognitoIdentity, "{}")
	h.Set(headerClientContext, "{}")

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(payload)),
		Header:     h,
	}

	inv, err := parseInvocation(resp)
	if err != nil {
		t.Fatalf("parseInvocation unexpected error: %v", err)
	}

	if inv.RequestID != "req-123" {
		t.Errorf("expected RequestID 'req-123', got '%s'", inv.RequestID)
	}
	if inv.InvokedFunctionArn != "arn:aws:lambda:us-east-1:123:function:test" {
		t.Errorf("unexpected ARN: %s", inv.InvokedFunctionArn)
	}
	if inv.Deadline.IsZero() {
		t.Error("expected non-zero Deadline from header")
	}
	if inv.TraceID == "" {
		t.Error("expected TraceID to be populated")
	}
	if string(inv.Payload) != string(payload) {
		t.Errorf("payload mismatch: expected %s, got %s", string(payload), string(inv.Payload))
	}
}
