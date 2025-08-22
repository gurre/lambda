// Package runtimeapi provides a high-performance AWS Lambda Runtime API client.
// This implementation is optimized for minimal latency with connection reuse,
// proper HTTP transport configuration, and optional prefetching capabilities.
//
// The client supports all standard Lambda Runtime API operations:
//   - Getting next invocations
//   - Sending responses
//   - Reporting errors
//   - Initialization error reporting
//   - Advanced prefetching for reduced cold start impact
//
// Environment Variables:
//
//	AWS_LAMBDA_RUNTIME_API - Required. Set automatically by Lambda runtime.
//	RUNTIME_DEBUG=1        - Optional. Enables detailed HTTP trace logging.
package runtimeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"time"
)

// runtimeAPIPrefix is the standard AWS Lambda Runtime API path prefix
// as defined in the Lambda Runtime API specification.
const runtimeAPIPrefix = "/2018-06-01/runtime"

// Lambda Runtime API headers used for metadata exchange between
// the Lambda service and custom runtimes.
const (
	// headerAWSRequestID contains the unique request identifier for each invocation.
	// Example: "8476a536-e9f4-11e8-9739-2dfe598c3fcd"
	headerAWSRequestID = "Lambda-Runtime-Aws-Request-Id"

	// headerDeadlineMS contains the invocation deadline in Unix milliseconds.
	// Example: "1542409706888"
	headerDeadlineMS = "Lambda-Runtime-Deadline-Ms"

	// headerTraceID contains the AWS X-Ray tracing information.
	// Example: "Root=1-5bef4de7-ad49b0e87f6ef6c87fc2e700;Parent=9a9197af755a6419;Sampled=1"
	headerTraceID = "Lambda-Runtime-Trace-Id"

	// headerCognitoIdentity contains Amazon Cognito identity data for mobile SDK invocations.
	headerCognitoIdentity = "Lambda-Runtime-Cognito-Identity"

	// headerClientContext contains client application data for mobile SDK invocations.
	headerClientContext = "Lambda-Runtime-Client-Context"

	// headerInvokedFunctionARN contains the ARN of the invoked Lambda function.
	// Example: "arn:aws:lambda:us-east-2:123456789012:function:my-function"
	headerInvokedFunctionARN = "Lambda-Runtime-Invoked-Function-Arn"
)

// RuntimeAPI defines the interface for AWS Lambda Runtime API operations.
// This interface provides the core methods needed to interact with the Lambda
// runtime environment for custom runtimes.
//
// Example usage:
//
//	client, err := NewClient()
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	for {
//		// Get next invocation
//		inv, err := client.Next()
//		if err != nil {
//			log.Printf("Error getting next invocation: %v", err)
//			continue
//		}
//
//		// Process the invocation
//		result, err := processInvocation(inv)
//		if err != nil {
//			// Send error response
//			errJSON := fmt.Sprintf(`{"errorMessage":%q}`, err.Error())
//			client.Error(inv.RequestID, []byte(errJSON))
//		} else {
//			// Send successful response
//			client.Response(inv.RequestID, result)
//		}
//	}
type RuntimeAPI interface {
	// Next retrieves the next invocation event from the Lambda Runtime API.
	// This method blocks until an invocation is available or an error occurs.
	//
	// Returns:
	//   - *Invocation: Contains the invocation data including payload and metadata
	//   - error: Network errors, API errors, or parsing failures
	//
	// Example:
	//   inv, err := client.Next()
	//   if err != nil {
	//       log.Printf("Failed to get next invocation: %v", err)
	//       return
	//   }
	//   fmt.Printf("Request ID: %s", inv.RequestID)
	Next() (*Invocation, error)

	// Response sends a successful response back to the Lambda Runtime API.
	// The payload should be the JSON-encoded result of the function execution.
	//
	// Parameters:
	//   - requestID: The request ID from the invocation (inv.RequestID)
	//   - payload: JSON-encoded response data
	//
	// Example:
	//   result := map[string]interface{}{"message": "Hello World"}
	//   jsonData, _ := json.Marshal(result)
	//   err := client.Response(inv.RequestID, jsonData)
	Response(requestID string, payload []byte) error

	// Error sends an error response back to the Lambda Runtime API.
	// The errBody should be a JSON object containing error information.
	//
	// Parameters:
	//   - requestID: The request ID from the invocation (inv.RequestID)
	//   - errBody: JSON-encoded error information
	//
	// Example:
	//   errorData := map[string]interface{}{
	//       "errorMessage": "Something went wrong",
	//       "errorType": "RuntimeError",
	//   }
	//   jsonData, _ := json.Marshal(errorData)
	//   err := client.Error(inv.RequestID, jsonData)
	Error(requestID string, errBody []byte) error

	// InitError sends an initialization error to the Lambda Runtime API.
	// This should be called during the initialization phase if setup fails.
	//
	// Parameters:
	//   - errBody: JSON-encoded error information
	//
	// Example:
	//   errorData := map[string]interface{}{
	//       "errorMessage": "Failed to initialize database connection",
	//       "errorType": "InitializationError",
	//   }
	//   jsonData, _ := json.Marshal(errorData)
	//   err := client.InitError(jsonData)
	InitError(errBody []byte) error
}

// lambdaTransport is a shared HTTP transport optimized for AWS Lambda Rapid communication.
// This transport is configured for optimal performance with the local Lambda runtime:
//   - No proxy configuration (Rapid is local loopback)
//   - Increased connection pooling to prevent head-of-line blocking
//   - Disabled compression and HTTP/2 (Rapid uses plain HTTP/1.1)
//   - Extended idle timeout for connection reuse
//   - Fast dial and keep-alive settings for low latency
var lambdaTransport = &http.Transport{
	// Rapid is loopback; never waste time consulting environment proxies
	Proxy: nil,
	// Increase from default 100 to handle burst workloads
	MaxIdleConns: 16,
	// Increase from default 2 to avoid head-of-line blocking stalls between GET /next and POST /response
	MaxIdleConnsPerHost: 16,
	// Allow connections to stay idle longer for better reuse
	IdleConnTimeout: 120 * time.Second,
	// Avoid transparent gzip compression overhead for local communication
	DisableCompression: true,
	// Rapid only supports HTTP/1.1
	ForceAttemptHTTP2: false,
	// Fast TCP keep-alive for local communication
	DialContext: (&net.Dialer{
		Timeout:   1 * time.Second,  // Fast connection timeout for local communication
		KeepAlive: 30 * time.Second, // TCP keep-alive
	}).DialContext,
	// Disable Expect: 100-continue for small payloads (reduces round trips)
	ExpectContinueTimeout: 0,
	// Other timeouts remain default; /next is long-poll controlled via Client.Timeout
}

// Specialized HTTP clients sharing the optimized transport.
// Using separate clients allows different timeout policies for different operations.
var (
	// nextClient handles long-polling /invocation/next requests with no timeout
	nextClient = &http.Client{Transport: lambdaTransport, Timeout: 0}
	// postClient handles quick POST operations (responses/errors) with default timeout
	postClient = &http.Client{Transport: lambdaTransport}
)

// Client implements the RuntimeAPI interface and provides an optimized
// AWS Lambda Runtime API client with connection reuse and prefetching capabilities.
//
// The client is designed for high-performance Lambda custom runtimes with:
//   - Shared HTTP transport for connection reuse
//   - Separate clients for different operation types
//   - Optional HTTP request tracing for debugging
//   - Proper connection draining for optimal reuse
type Client struct {
	// baseURL is the full base URL for Runtime API calls
	// Format: http://$AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime
	baseURL string
}

// NewClient creates a new Lambda Runtime API client.
// The client reads the runtime API endpoint from the AWS_LAMBDA_RUNTIME_API
// environment variable, which is automatically set by the Lambda service.
//
// Returns:
//   - *Client: A configured client ready for use
//   - error: If AWS_LAMBDA_RUNTIME_API environment variable is not set
//
// Example:
//
//	client, err := NewClient()
//	if err != nil {
//	    log.Fatalf("Failed to create runtime client: %v", err)
//	}
//
//	// Client is now ready for use
//	inv, err := client.Next()
func NewClient() (*Client, error) {
	host := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if host == "" {
		return nil, errors.New("AWS_LAMBDA_RUNTIME_API environment variable not set")
	}
	return &Client{
		baseURL: "http://" + host + runtimeAPIPrefix,
	}, nil
}

// Invocation represents a Lambda function invocation event received from
// the Runtime API. It contains all the metadata and payload needed to
// process the function execution.
//
// Example usage:
//
//	inv, err := client.Next()
//	if err != nil {
//	    return err
//	}
//
//	// Access invocation data
//	fmt.Printf("Request ID: %s\n", inv.RequestID)
//	fmt.Printf("Function ARN: %s\n", inv.InvokedFunctionArn)
//	fmt.Printf("Deadline: %s\n", inv.Deadline.Format(time.RFC3339))
//
//	// Process the payload
//	var event map[string]interface{}
//	json.Unmarshal(inv.Payload, &event)
type Invocation struct {
	// RequestID is the unique identifier for this invocation.
	// This must be used when sending responses or errors.
	// Example: "8476a536-e9f4-11e8-9739-2dfe598c3fcd"
	RequestID string

	// InvokedFunctionArn is the ARN of the Lambda function being invoked.
	// Example: "arn:aws:lambda:us-east-2:123456789012:function:my-function"
	InvokedFunctionArn string

	// Deadline is when the function execution must complete.
	// The Lambda service will terminate the execution after this time.
	Deadline time.Time

	// TraceID contains AWS X-Ray tracing information for distributed tracing.
	// Example: "Root=1-5bef4de7-ad49b0e87f6ef6c87fc2e700;Parent=9a9197af755a6419;Sampled=1"
	TraceID string

	// CognitoIdentity contains Amazon Cognito identity information
	// when the function is invoked from AWS Mobile SDK.
	CognitoIdentity string

	// ClientContext contains client application information
	// when the function is invoked from AWS Mobile SDK.
	ClientContext string

	// Payload contains the raw invocation event data as JSON bytes.
	// This should be unmarshaled into your event structure.
	Payload []byte

	// Headers contains all HTTP headers from the invocation request.
	// Useful for accessing additional metadata or custom headers.
	Headers http.Header
}

// ---- Internal Helpers ----------------------------------------------------------

// drainAndClose ensures the HTTP response body is fully read and closed.
// This is critical for HTTP connection reuse - if the body isn't fully
// drained, the connection cannot be reused and will be closed.
//
// This function is called in defer statements to guarantee proper cleanup
// regardless of how the response processing completes.
func drainAndClose(b io.ReadCloser) {
	if b == nil {
		return
	}
	// Drain any remaining data to enable connection reuse
	_, _ = io.Copy(io.Discard, b)
	_ = b.Close()
}

// parseDeadline extracts and converts the deadline from HTTP headers.
// The Lambda Runtime API provides deadlines as Unix milliseconds in the
// Lambda-Runtime-Deadline-Ms header. This function converts that to time.Time.
//
// Returns:
//   - time.Time: Parsed deadline, or zero time if parsing fails
func parseDeadline(h http.Header) time.Time {
	if msStr := h.Get(headerDeadlineMS); msStr != "" {
		if ms, err := strconv.ParseInt(msStr, 10, 64); err == nil {
			// Convert Unix milliseconds to time.Time
			return time.Unix(0, ms*int64(time.Millisecond))
		}
	}
	// Return zero time if parsing fails or header is missing
	return time.Time{}
}

// parseInvocation parses an HTTP response from /invocation/next into an Invocation struct.
// It extracts metadata from headers and reads the payload from the response body.
//
// This function handles the complete parsing of Lambda Runtime API responses including:
//   - Status code validation
//   - Header extraction for all metadata fields
//   - Payload reading
//   - Proper error formatting for debugging
//
// Parameters:
//   - resp: HTTP response from the /invocation/next endpoint
//
// Returns:
//   - *Invocation: Parsed invocation data with all metadata
//   - error: Parsing errors, HTTP errors, or I/O errors
func parseInvocation(resp *http.Response) (*Invocation, error) {
	// Ensure body is properly drained for connection reuse
	defer drainAndClose(resp.Body)

	// Check for non-200 status codes
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("invocation/next failed: %s: %s", resp.Status, string(body))
	}

	// Read the invocation payload
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read invocation payload: %w", err)
	}

	// Extract metadata from headers
	h := resp.Header
	return &Invocation{
		RequestID:          h.Get(headerAWSRequestID),
		InvokedFunctionArn: h.Get(headerInvokedFunctionARN),
		Deadline:           parseDeadline(h),
		TraceID:            h.Get(headerTraceID),
		CognitoIdentity:    h.Get(headerCognitoIdentity),
		ClientContext:      h.Get(headerClientContext),
		Payload:            payload,
		Headers:            h.Clone(), // Clone headers to avoid data races
	}, nil
}

// ---- Public Runtime API Methods ------------------------------------------------

// Next retrieves the next invocation from the Lambda Runtime API.
// This method implements a blocking call that waits for the next function
// invocation to be available. It's the primary method for receiving work
// in a Lambda custom runtime.
//
// The method will block indefinitely until:
//   - An invocation is received
//   - A network error occurs
//   - The Lambda service shuts down the runtime
//
// Returns:
//   - *Invocation: Complete invocation data with payload and metadata
//   - error: Network errors, API errors, or response parsing failures
//
// Example:
//
//	for {
//	    inv, err := client.Next()
//	    if err != nil {
//	        log.Printf("Error getting next invocation: %v", err)
//	        time.Sleep(time.Second) // Brief pause before retry
//	        continue
//	    }
//
//	    // Process the invocation
//	    processInvocation(inv)
//	}
func (c *Client) Next() (*Invocation, error) {
	return c.getNextInvocation(context.Background())
}

// getNextInvocation is the internal implementation of Next() with context support.
// This allows for advanced usage patterns like timeouts or cancellation,
// though the public Next() method uses a background context for simplicity.
func (c *Client) getNextInvocation(ctx context.Context) (*Invocation, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/invocation/next", nil)

	resp, err := nextClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get next invocation: %w", err)
	}

	return parseInvocation(resp)
}

// Response sends a successful response for a Lambda invocation.
// This method should be called after successfully processing an invocation
// to return the result to the Lambda service.
//
// Parameters:
//   - requestID: The RequestID from the Invocation (inv.RequestID)
//   - payload: JSON-encoded response data (the function's return value)
//
// The payload should be valid JSON. For simple responses:
//
//	result := map[string]string{"message": "Hello, World!"}
//	jsonData, _ := json.Marshal(result)
//	err := client.Response(inv.RequestID, jsonData)
//
// For binary data, use base64 encoding:
//
//	encoded := base64.StdEncoding.EncodeToString(binaryData)
//	response := map[string]interface{}{
//	    "statusCode": 200,
//	    "body": encoded,
//	    "isBase64Encoded": true,
//	}
//	jsonData, _ := json.Marshal(response)
//	err := client.Response(inv.RequestID, jsonData)
//
// Returns:
//   - error: Network errors, API errors, or HTTP status errors
func (c *Client) Response(requestID string, payload []byte) error {
	if requestID == "" {
		return errors.New("requestID cannot be empty")
	}
	return c.postResponse(context.Background(), requestID, payload)
}

// postResponse is the internal implementation of Response() with context support.
func (c *Client) postResponse(ctx context.Context, requestID string, payload []byte) error {
	url := c.baseURL + "/invocation/" + requestID + "/response"
	return c.postCommon(ctx, url, payload)
}

// Error sends an error response for a Lambda invocation.
// This method should be called when an invocation fails due to an error
// in your function logic, invalid input, or other processing failures.
//
// Parameters:
//   - requestID: The RequestID from the Invocation (inv.RequestID)
//   - errBody: JSON-encoded error information
//
// The errBody should contain error details in a structured format:
//
//	errorInfo := map[string]interface{}{
//	    "errorMessage": "Invalid input: missing required field 'name'",
//	    "errorType": "ValidationError",
//	    "stackTrace": []string{
//	        "main.processEvent(main.go:45)",
//	        "main.handleInvocation(main.go:23)",
//	    },
//	}
//	jsonData, _ := json.Marshal(errorInfo)
//	err := client.Error(inv.RequestID, jsonData)
//
// For simple errors:
//
//	simpleError := map[string]string{
//	    "errorMessage": err.Error(),
//	    "errorType": "RuntimeError",
//	}
//	jsonData, _ := json.Marshal(simpleError)
//	err := client.Error(inv.RequestID, jsonData)
//
// Returns:
//   - error: Network errors, API errors, or HTTP status errors
func (c *Client) Error(requestID string, errBody []byte) error {
	if requestID == "" {
		return errors.New("requestID cannot be empty")
	}
	return c.postError(context.Background(), requestID, errBody)
}

// postError is the internal implementation of Error() with context support.
func (c *Client) postError(ctx context.Context, requestID string, errJSON []byte) error {
	url := c.baseURL + "/invocation/" + requestID + "/error"
	return c.postCommon(ctx, url, errJSON)
}

// InitError sends an initialization error to the Lambda Runtime API.
// This method should be called during the initialization phase if your
// runtime fails to set up properly (e.g., database connections, configuration
// loading, dependency initialization).
//
// After calling InitError, the Lambda runtime will be terminated, so this
// should only be used for fatal initialization failures.
//
// Parameters:
//   - errBody: JSON-encoded error information describing the initialization failure
//
// Example usage during runtime initialization:
//
//	func init() {
//	    db, err := sql.Open("postgres", connectionString)
//	    if err != nil {
//	        client, _ := runtimeapi.NewClient()
//	        errorInfo := map[string]string{
//	            "errorMessage": fmt.Sprintf("Failed to connect to database: %v", err),
//	            "errorType": "DatabaseConnectionError",
//	        }
//	        jsonData, _ := json.Marshal(errorInfo)
//	        client.InitError(jsonData)
//	        os.Exit(1)
//	    }
//	}
//
// For configuration errors:
//
//	if config.APIKey == "" {
//	    errorInfo := map[string]interface{}{
//	        "errorMessage": "Missing required environment variable: API_KEY",
//	        "errorType": "ConfigurationError",
//	    }
//	    jsonData, _ := json.Marshal(errorInfo)
//	    err := client.InitError(jsonData)
//	}
//
// Returns:
//   - error: Network errors, API errors, or HTTP status errors
func (c *Client) InitError(errBody []byte) error {
	url := c.baseURL + "/init/error"
	return c.postCommon(context.Background(), url, errBody)
}

// postCommon handles the common POST request logic for responses, errors, and init errors.
// This internal method provides consistent error handling, debugging, and connection management
// for all POST operations to the Lambda Runtime API.
//
// Features:
//   - Consistent HTTP headers and user agent
//   - Optional HTTP tracing for debugging
//   - Proper connection management via body draining
//   - Standardized error formatting
//   - Content-Length header to avoid chunked encoding
//
// Parameters:
//   - ctx: Context for request cancellation and timeouts
//   - url: Complete URL for the POST request
//   - body: Request body (typically JSON-encoded data)
//
// Returns:
//   - error: HTTP errors, network errors, or API errors
func (c *Client) postCommon(ctx context.Context, url string, body []byte) error {
	// Use bytes.NewReader to enable Content-Length header (avoids chunked encoding)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	// Explicitly set Content-Length to avoid chunked transfer encoding
	req.ContentLength = int64(len(body))

	// Enable detailed HTTP tracing if debug mode is enabled
	if debugOn() {
		req = withHTTPTrace(ctx, req, "POST")
	}

	resp, err := postClient.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}

	// Always drain and close the response body for connection reuse
	defer drainAndClose(resp.Body)

	// Check for HTTP error status codes
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s failed: %s: %s", url, resp.Status, string(b))
	}

	return nil
}

// ---- Advanced Performance Features ---------------------------------------------

// PrefetchNext starts a concurrent request for the next invocation while you
// process the current one. This advanced feature can reduce latency by overlapping
// network I/O with function execution, especially beneficial for high-throughput
// scenarios or when you want to minimize cold start impact.
//
// The method returns two channels:
//   - <-chan *Invocation: Receives the next invocation when available
//   - <-chan error: Receives any error that occurred during prefetch
//
// Important considerations:
//   - Only one of the channels will receive a value
//   - The goroutine will complete when either channel receives data
//   - Use this pattern to overlap processing with network I/O
//   - Don't forget to handle both channels to avoid goroutine leaks
//
// Example usage for latency optimization:
//
//	// Start prefetch immediately after receiving current invocation
//	nextCh, errCh := client.PrefetchNext()
//
//	// Process current invocation
//	result, err := processInvocation(currentInv)
//
//	// Send response for current invocation
//	if err != nil {
//	    client.Error(currentInv.RequestID, errorJSON)
//	} else {
//	    client.Response(currentInv.RequestID, result)
//	}
//
//	// Now await the prefetched next invocation
//	select {
//	case nextInv := <-nextCh:
//	    // Process nextInv immediately
//	    processInvocation(nextInv)
//	case err := <-errCh:
//	    // Handle prefetch error, fall back to regular Next()
//	    log.Printf("Prefetch failed: %v", err)
//	    nextInv, err := client.Next()
//	}
//
// Returns:
//   - <-chan *Invocation: Channel that will receive the next invocation
//   - <-chan error: Channel that will receive any prefetch errors
func (c *Client) PrefetchNext() (<-chan *Invocation, <-chan error) {
	// Use buffered channels to prevent goroutine blocking
	out := make(chan *Invocation, 1)
	er := make(chan error, 1)

	go func() {
		ctx := context.Background()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/invocation/next", nil)

		// Enable tracing for prefetch requests if debugging
		if debugOn() {
			req = withHTTPTrace(ctx, req, "GET(prefetch)")
		}

		resp, err := nextClient.Do(req)
		if err != nil {
			er <- fmt.Errorf("prefetch request failed: %w", err)
			return
		}

		inv, perr := parseInvocation(resp)
		if perr != nil {
			er <- fmt.Errorf("prefetch parsing failed: %w", perr)
			return
		}

		out <- inv
	}()

	return out, er
}

// ---- Debug and Monitoring Features ---------------------------------------------

// debugOn checks if detailed HTTP tracing is enabled via environment variable.
// Set RUNTIME_DEBUG=1 to enable detailed logging of HTTP connection behavior,
// which is useful for debugging connection reuse, performance issues, and
// network behavior in Lambda custom runtimes.
//
// Example debug output when enabled:
//
//	[POST] GetConn 127.0.0.1:9001
//	[POST] GotConn reused=true wasIdle=true idleTime=45.2ms
//	[POST] WroteRequest err=<nil>
//	[POST] GotFirstResponseByte
//
// Returns:
//   - bool: true if RUNTIME_DEBUG=1, false otherwise
func debugOn() bool {
	return os.Getenv("RUNTIME_DEBUG") == "1"
}

// withHTTPTrace adds detailed HTTP connection tracing to a request.
// This is used for debugging connection reuse, performance issues, and
// network behavior when RUNTIME_DEBUG=1 is set.
//
// The trace logs include:
//   - Connection acquisition and reuse status
//   - Request writing completion
//   - Response timing information
//
// This is particularly useful for diagnosing:
//   - Connection pool behavior and reuse efficiency
//   - Network latency and timing issues
//   - HTTP transport configuration problems
//   - Lambda Rapid communication patterns
//
// Parameters:
//   - ctx: Context for the HTTP request
//   - req: HTTP request to add tracing to
//   - tag: Label for identifying the request type in logs (e.g., "POST", "GET(prefetch)")
//
// Returns:
//   - *http.Request: Request with HTTP trace context attached
//
// Example debug output:
//
//	[POST] GetConn 127.0.0.1:9001
//	[POST] GotConn reused=true wasIdle=true idleTime=45.2ms
//	[POST] WroteRequest err=<nil>
//	[POST] GotFirstResponseByte
func withHTTPTrace(ctx context.Context, req *http.Request, tag string) *http.Request {
	trace := &httptrace.ClientTrace{
		// Log when getting a connection from the pool
		GetConn: func(hostPort string) {
			log.Printf("[%s] GetConn %s", tag, hostPort)
		},
		// Log connection details - this shows if connection reuse is working
		GotConn: func(info httptrace.GotConnInfo) {
			log.Printf("[%s] GotConn reused=%v wasIdle=%v idleTime=%s",
				tag, info.Reused, info.WasIdle, info.IdleTime)
		},
		// Log request writing completion
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			log.Printf("[%s] WroteRequest err=%v", tag, info.Err)
		},
		// Log when response starts arriving
		GotFirstResponseByte: func() {
			log.Printf("[%s] GotFirstResponseByte", tag)
		},
	}
	return req.WithContext(httptrace.WithClientTrace(ctx, trace))
}
