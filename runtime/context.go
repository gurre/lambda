package runtime

import (
	"context"
	"os"
	"strconv"
	"sync"
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

// Pool for RequestContext structs to reduce allocations in high-frequency scenarios
var requestContextPool = sync.Pool{
	New: func() interface{} {
		return &RequestContext{}
	},
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

	// AWS Lambda environment metadata (cached after first population)
	AWSRegion          string // AWS Region where the Lambda function is executed
	AWSDefaultRegion   string // Default AWS Region
	FunctionName       string // Name of the current Lambda function
	FunctionVersion    string // Published version of the current Lambda
	LogGroupName       string // CloudWatch Logs group for this Lambda
	LogStreamName      string // CloudWatch Logs stream for this Lambda instance
	MemoryLimitInMB    int    // Configured memory limit for this Lambda instance
	AWSExecutionEnv    string // Runtime identifier (e.g., AWS_Lambda_go1.x)
	InitializationType string // Initialization type (on-demand, provisioned-concurrency, snap-start)
	
	// Performance optimization: flag to track if environment was already populated
	envPopulated       bool   // Internal flag to avoid repeated environment reads
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

// Cached environment variables to avoid repeated os.Getenv calls
var (
	envCache struct {
		sync.Once
		awsRegion          string
		awsDefaultRegion   string
		functionName       string
		functionVersion    string
		logGroupName       string
		logStreamName      string
		awsExecutionEnv    string
		initializationType string
		memoryLimitInMB    int
	}
)

// initEnvironmentCache initializes the environment cache once
func initEnvironmentCache() {
	envCache.awsRegion = os.Getenv("AWS_REGION")
	envCache.awsDefaultRegion = os.Getenv("AWS_DEFAULT_REGION")
	envCache.functionName = os.Getenv("AWS_LAMBDA_FUNCTION_NAME")
	envCache.functionVersion = os.Getenv("AWS_LAMBDA_FUNCTION_VERSION")
	envCache.logGroupName = os.Getenv("AWS_LAMBDA_LOG_GROUP_NAME")
	envCache.logStreamName = os.Getenv("AWS_LAMBDA_LOG_STREAM_NAME")
	envCache.awsExecutionEnv = os.Getenv("AWS_EXECUTION_ENV")
	envCache.initializationType = os.Getenv("AWS_LAMBDA_INITIALIZATION_TYPE")
	
	// Parse memory limit once
	if limit, err := strconv.Atoi(os.Getenv("AWS_LAMBDA_FUNCTION_MEMORY_SIZE")); err == nil {
		envCache.memoryLimitInMB = limit
	}
}

// PopulateFromEnvironment populates the RequestContext with AWS Lambda
// environment metadata. Optimized to avoid repeated os.Getenv calls by using
// a cached approach. Safe to call multiple times.
func (rc *RequestContext) PopulateFromEnvironment() {
	// Initialize environment cache only once
	envCache.Do(initEnvironmentCache)
	
	// Copy from cache instead of calling os.Getenv repeatedly
	rc.AWSRegion = envCache.awsRegion
	rc.AWSDefaultRegion = envCache.awsDefaultRegion
	rc.FunctionName = envCache.functionName
	rc.FunctionVersion = envCache.functionVersion
	rc.LogGroupName = envCache.logGroupName
	rc.LogStreamName = envCache.logStreamName
	rc.AWSExecutionEnv = envCache.awsExecutionEnv
	rc.InitializationType = envCache.initializationType
	rc.MemoryLimitInMB = envCache.memoryLimitInMB
	
	rc.envPopulated = true
}

// GetPooledRequestContext returns a RequestContext from the pool with environment pre-populated
func GetPooledRequestContext() *RequestContext {
	rc := requestContextPool.Get().(*RequestContext)
	
	// Reset per-invocation fields
	rc.AwsRequestID = ""
	rc.InvokedFunctionArn = ""
	rc.Deadline = time.Time{}
	rc.TraceID = ""
	rc.Identity = CognitoIdentity{}
	rc.ClientContext = ClientContext{}
	
	// Populate environment if not already done
	if !rc.envPopulated {
		rc.PopulateFromEnvironment()
	}
	
	return rc
}

// ReturnPooledRequestContext returns a RequestContext to the pool
func ReturnPooledRequestContext(rc *RequestContext) {
	// Keep environment metadata but clear per-invocation fields
	rc.AwsRequestID = ""
	rc.InvokedFunctionArn = ""
	rc.Deadline = time.Time{}
	rc.TraceID = ""
	rc.Identity = CognitoIdentity{}
	rc.ClientContext = ClientContext{}
	
	requestContextPool.Put(rc)
}
