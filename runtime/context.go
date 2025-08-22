package runtime

import (
	"context"
	"os"
	"strconv"
	"time"
)

// All AWS Lambda environment metadata is now stored in the RequestContext.
// No global variables are used - everything is accessed through context.

// ClientApplication is metadata about the calling application.
type ClientApplication struct {
	InstallationID string `json:"installation_id"`
	AppTitle       string `json:"app_title"`
	AppVersionCode string `json:"app_version_code"`
	AppPackageName string `json:"app_package_name"`
}

// ClientContext contains information about the client application passed by the invoker.
type ClientContext struct {
	Client ClientApplication `json:"client"`
	Env    map[string]string `json:"env"`
	Custom map[string]string `json:"custom"`
}

// CognitoIdentity captures the Cognito identity used by the caller.
type CognitoIdentity struct {
	CognitoIdentityID     string `json:"cognitoIdentityId"`
	CognitoIdentityPoolID string `json:"cognitoIdentityPoolId"`
}

// RequestContext carries invocation metadata and AWS Lambda environment information.
type RequestContext struct {
	// Per-invocation fields
	AwsRequestID       string
	InvokedFunctionArn string
	Deadline           time.Time
	TraceID            string
	Identity           CognitoIdentity
	ClientContext      ClientContext

	// AWS Lambda environment metadata
	AWSRegion          string // AWS Region where the Lambda function is executed
	AWSDefaultRegion   string // Default AWS Region
	FunctionName       string // Name of the current Lambda function
	FunctionVersion    string // Published version of the current Lambda
	LogGroupName       string // CloudWatch Logs group for this Lambda
	LogStreamName      string // CloudWatch Logs stream for this Lambda instance
	MemoryLimitInMB    int    // Configured memory limit for this Lambda instance
	AWSExecutionEnv    string // Runtime identifier (e.g., AWS_Lambda_go1.x)
	InitializationType string // Initialization type (on-demand, provisioned-concurrency, snap-start)
}

// Use a string key to avoid circular import issues with context access
const lambdaContextKey = "RequestContext"

// NewContext returns a new context that carries the provided RequestContext.
//
// It stores a pointer to the RequestContext which allows the caller to reuse a
// single struct across invocations by updating only per-invocation fields
// (environment metadata can be pre-populated once).
func NewContext(parent context.Context, lc *RequestContext) context.Context {
	return context.WithValue(parent, lambdaContextKey, lc)
}

// FromContext retrieves the RequestContext stored in ctx, if any.
// It returns (nil, false) if no RequestContext has been stored.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	lc, ok := ctx.Value(lambdaContextKey).(*RequestContext)
	return lc, ok
}

// WithRequestContext stores the provided RequestContext by value-copy in a new
// context. Mutating the original value after calling this function will not
// affect the stored context value.
func WithRequestContext(parent context.Context, lc RequestContext) context.Context {
	copy := lc
	return NewContext(parent, &copy)
}

// PopulateFromEnvironment populates the RequestContext with AWS Lambda
// environment metadata. Safe to call multiple times; values are overwritten
// with current environment variables.
func (rc *RequestContext) PopulateFromEnvironment() {
	rc.AWSRegion = os.Getenv("AWS_REGION")
	rc.AWSDefaultRegion = os.Getenv("AWS_DEFAULT_REGION")
	rc.FunctionName = os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	rc.FunctionVersion = os.Getenv("AWS_LAMBDA_FUNCTION_VERSION")
	rc.LogGroupName = os.Getenv("AWS_LAMBDA_LOG_GROUP_NAME")
	rc.LogStreamName = os.Getenv("AWS_LAMBDA_LOG_STREAM_NAME")
	rc.AWSExecutionEnv = os.Getenv("AWS_EXECUTION_ENV")
	rc.InitializationType = os.Getenv("AWS_LAMBDA_INITIALIZATION_TYPE")

	// Parse memory limit
	if limit, err := strconv.Atoi(os.Getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE")); err == nil {
		rc.MemoryLimitInMB = limit
	}
}
