#!/bin/bash

set -e  # Exit on any error

# Configuration
DEFAULT_REGION="us-east-1"
ZIP_FILE="function.zip"
BOOTSTRAP_BINARY="bootstrap"
TEST_PAYLOAD='{"input": "Hello, Lambda Runtime!"}'

# Static resource names (no more state file needed)
FUNCTION_NAME="lambda-runtime-test"
ROLE_NAME="lambda-runtime-test-role"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Get current AWS region
get_region() {
    aws configure get region 2>/dev/null || echo "$DEFAULT_REGION"
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    # Check if AWS CLI is installed
    if ! command -v aws &> /dev/null; then
        log_error "AWS CLI is required but not installed"
        exit 1
    fi
    
    # Check if Go is installed
    if ! command -v go &> /dev/null; then
        log_error "Go is required but not installed"
        exit 1
    fi
    
    # Check if bc is installed (needed for benchmarking)
    if ! command -v bc &> /dev/null; then
        log_warning "bc (basic calculator) not found - benchmarking will not work"
        log_info "Install bc with: brew install bc (macOS) or apt-get install bc (Ubuntu)"
    fi
    
    # Check AWS credentials
    if ! aws sts get-caller-identity >/dev/null 2>&1; then
        log_error "AWS credentials not configured. Run 'aws configure' first"
        exit 1
    fi
    
    log_success "Prerequisites check passed"
}

# Build the Lambda function
build_function() {
    log_info "Building Lambda function..."
    
    # Build for Linux (Lambda runtime environment)
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o "$BOOTSTRAP_BINARY" ./cmd/bootstrap/
    
    if [[ ! -f "$BOOTSTRAP_BINARY" ]]; then
        log_error "Failed to build bootstrap binary"
        exit 1
    fi
    
    log_success "Lambda function built successfully"
}

# Create deployment package
create_zip() {
    log_info "Creating deployment package..."
    
    # Remove existing zip file
    rm -f "$ZIP_FILE"
    
    # Create zip file with bootstrap binary
    zip -q "$ZIP_FILE" "$BOOTSTRAP_BINARY"
    
    if [[ ! -f "$ZIP_FILE" ]]; then
        log_error "Failed to create deployment package"
        exit 1
    fi
    
    local size=$(ls -lh "$ZIP_FILE" | awk '{print $5}')
    log_success "Deployment package created: $ZIP_FILE ($size)"
}

# Create IAM role for Lambda
create_iam_role() {
    local region="$1"
    
    log_info "Creating IAM role for Lambda..." >&2
    
    # Create trust policy
    local trust_policy='{
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Principal": {
                    "Service": "lambda.amazonaws.com"
                },
                "Action": "sts:AssumeRole"
            }
        ]
    }'
    
    # Create role
    aws iam create-role \
        --role-name "$ROLE_NAME" \
        --assume-role-policy-document "$trust_policy" \
        --region "$region" >/dev/null
    
    # Attach basic execution policy
    aws iam attach-role-policy \
        --role-name "$ROLE_NAME" \
        --policy-arn "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole" \
        --region "$region" >/dev/null
    
    # Get account ID for role ARN
    local account_id=$(aws sts get-caller-identity --query Account --output text)
    local role_arn="arn:aws:iam::${account_id}:role/${ROLE_NAME}"
    
    log_success "IAM role created: $role_arn" >&2
    
    # Wait for role to be available
    log_info "Waiting for IAM role to be ready..." >&2
    sleep 10
    
    # Only output the role ARN to stdout
    echo "$role_arn"
}

# Create Lambda function
create_lambda_function() {
    local role_arn="$1"
    local region="$2"
    local app_log_level="${3:-INFO}"
    local system_log_level="${4:-INFO}"
    
    log_info "Creating Lambda function with JSON logging..."
    log_info "Application Log Level: $app_log_level"
    log_info "System Log Level: $system_log_level"
    
    aws lambda create-function \
        --function-name "$FUNCTION_NAME" \
        --runtime "provided.al2023" \
        --role "$role_arn" \
        --handler "bootstrap" \
        --zip-file "fileb://$ZIP_FILE" \
        --timeout 30 \
        --memory-size 128 \
        --region "$region" \
        --environment Variables="{LAMBDA_RUNTIME_TEST=true,LOG_FORMAT=JSON,LOG_LEVEL=$app_log_level,RUNTIME_DEBUG=1}" \
        --logging-config "LogFormat=JSON,ApplicationLogLevel=$app_log_level,SystemLogLevel=$system_log_level" >/dev/null
    
    log_success "Lambda function created: $FUNCTION_NAME"
    
    # Wait for function to be active
    log_info "Waiting for Lambda function to be ready..."
    aws lambda wait function-active --function-name "$FUNCTION_NAME" --region "$region"
}

# Invoke Lambda function
invoke_lambda() {
    local region="$1"
    local payload="${2:-$TEST_PAYLOAD}"
    
    log_info "Invoking Lambda function with test payload..."
    
    local response_file="lambda-response.json"
    local log_file="lambda-logs.txt"
    
    # Invoke function and capture logs
    aws lambda invoke \
        --function-name "$FUNCTION_NAME" \
        --payload "$payload" \
        --log-type Tail \
        --region "$region" \
        --cli-binary-format raw-in-base64-out \
        "$response_file" > "$log_file" 2>&1
    
    log_success "Lambda function invoked successfully"
    
    # Display response
    echo
    log_info "Lambda Response:"
    echo "----------------------------------------"
    if [[ -f "$response_file" ]]; then
        cat "$response_file" | python3 -m json.tool 2>/dev/null || cat "$response_file"
    fi
    echo
    echo "----------------------------------------"
    
    # Extract and decode logs from response
    if [[ -f "$log_file" ]]; then
        local encoded_logs=$(grep -o '"LogResult": "[^"]*"' "$log_file" | cut -d'"' -f4)
        if [[ -n "$encoded_logs" ]]; then
            echo
            log_info "Lambda Execution Logs:"
            echo "----------------------------------------"
            echo "$encoded_logs" | base64 --decode
            echo "----------------------------------------"
        fi
    fi
    
    # Cleanup temporary files
    rm -f "$response_file" "$log_file"
}

# Get CloudWatch logs
get_cloudwatch_logs() {
    local region="$1"
    local lines="${2:-50}"
    
    log_info "Retrieving CloudWatch logs..."
    
    local log_group="/aws/lambda/$FUNCTION_NAME"
    local start_time=$(($(date +%s) - 300))  # Last 5 minutes
    local end_time=$(date +%s)
    
    # Convert to milliseconds
    start_time=$((start_time * 1000))
    end_time=$((end_time * 1000))
    
    # Wait a bit for logs to appear
    sleep 5
    
    # Get log streams
    local log_streams=$(aws logs describe-log-streams \
        --log-group-name "$log_group" \
        --order-by LastEventTime \
        --descending \
        --max-items 1 \
        --region "$region" \
        --query 'logStreams[0].logStreamName' \
        --output text 2>/dev/null)
    
    if [[ "$log_streams" != "None" ]] && [[ -n "$log_streams" ]]; then
        echo
        log_info "CloudWatch Logs (Stream: $log_streams):"
        echo "----------------------------------------"
        
        aws logs get-log-events \
            --log-group-name "$log_group" \
            --log-stream-name "$log_streams" \
            --start-time "$start_time" \
            --end-time "$end_time" \
            --region "$region" \
            --query 'events[*].message' \
            --output text 2>/dev/null || log_warning "Could not retrieve CloudWatch logs"
        
        echo "----------------------------------------"
    else
        log_warning "No CloudWatch logs found yet. This is normal for the first execution."
    fi
}

# Command: create
cmd_create() {
    local app_log_level="${1:-INFO}"
    local system_log_level="${2:-INFO}"
    
    echo
    log_info "Creating Lambda function and resources"
    echo "====================================="
    
    local region=$(get_region)
    
    # Check if function already exists
    if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_warning "Lambda function already exists: $FUNCTION_NAME"
        log_info "Use '$0 cleanup' first to remove existing resources"
        exit 1
    fi
    
    check_prerequisites
    build_function
    create_zip
    
    # Create AWS resources
    local role_arn=$(create_iam_role "$region")
    create_lambda_function "$role_arn" "$region" "$app_log_level" "$system_log_level"
    
    echo
    log_success "Lambda function created successfully!"
    echo
    log_info "Resource Summary:"
    echo "  Function Name: $FUNCTION_NAME"
    echo "  Role Name: $ROLE_NAME"
    echo "  Region: $region"
    echo "  Log Format: JSON"
    echo "  Application Log Level: $app_log_level"
    echo "  System Log Level: $system_log_level"
    echo "  Created At: $(date)"
    echo
    log_info "Use '$0 invoke' to test the function"
    echo "Use '$0 cleanup' to remove all resources when done"
}

# Command: deploy
cmd_deploy() {
    echo
    log_info "Deploying updated Lambda function code"
    echo "======================================"
    
    local region=$(get_region)
    
    # Check if function exists
    if ! aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_error "Lambda function '$FUNCTION_NAME' not found. Use '$0 create' first"
        exit 1
    fi
    
    check_prerequisites
    build_function
    create_zip
    
    log_info "Updating Lambda function code..."
    
    # Update function code
    aws lambda update-function-code \
        --function-name "$FUNCTION_NAME" \
        --zip-file "fileb://$ZIP_FILE" \
        --region "$region" >/dev/null
    
    log_success "Lambda function code updated successfully!"
    
    # Wait for function to be ready
    log_info "Waiting for function update to complete..."
    aws lambda wait function-updated --function-name "$FUNCTION_NAME" --region "$region"
    
    echo
    log_info "Function Summary:"
    echo "  Function Name: $FUNCTION_NAME"
    echo "  Region: $region"
    echo "  Updated At: $(date)"
    echo
    log_info "Use '$0 invoke' to test the updated function"
}

# Command: invoke
cmd_invoke() {
    local payload="${1:-$TEST_PAYLOAD}"
    
    echo
    log_info "Invoking Lambda function"
    echo "========================"
    
    local region=$(get_region)
    
    # Check if function exists
    if ! aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_error "Lambda function '$FUNCTION_NAME' not found. Use '$0 create' first"
        exit 1
    fi
    
    log_info "Function: $FUNCTION_NAME"
    log_info "Region: $region"
    log_info "Payload: $payload"
    echo
    
    invoke_lambda "$region" "$payload"
    get_cloudwatch_logs "$region"
    
    echo
    log_success "Lambda invocation completed!"
}

# Command: cleanup
cmd_cleanup() {
    echo
    log_info "Cleaning up Lambda resources"
    echo "============================="
    
    local region=$(get_region)
    
    log_info "Function: $FUNCTION_NAME"
    log_info "Role: $ROLE_NAME"
    log_info "Region: $region"
    echo
    
    # Delete Lambda function
    if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_info "Deleting Lambda function: $FUNCTION_NAME"
        aws lambda delete-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1 || true
        log_success "Lambda function deleted"
    else
        log_warning "Lambda function not found (may have been deleted already)"
    fi
    
    # Delete IAM role (detach policies first)
    if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
        log_info "Detaching policies from role: $ROLE_NAME"
        aws iam detach-role-policy --role-name "$ROLE_NAME" --policy-arn "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole" >/dev/null 2>&1 || true
        
        log_info "Deleting IAM role: $ROLE_NAME"
        aws iam delete-role --role-name "$ROLE_NAME" >/dev/null 2>&1 || true
        log_success "IAM role deleted"
    else
        log_warning "IAM role not found (may have been deleted already)"
    fi
    
    # Remove local files
    rm -f "$ZIP_FILE" "$BOOTSTRAP_BINARY"
    
    echo
    log_success "Cleanup completed successfully!"
}

# Command: status
cmd_status() {
    echo
    log_info "Lambda function status"
    echo "====================="
    
    local region=$(get_region)
    
    echo "Function Name: $FUNCTION_NAME"
    echo "Role Name: $ROLE_NAME"
    echo "Region: $region"
    echo
    
    # Check if function exists
    if aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_success "Lambda function is active"
        
        # Get function details
        aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" \
            --query 'Configuration.{State:State,LastModified:LastModified,Runtime:Runtime,MemorySize:MemorySize,Timeout:Timeout}' \
            --output table
    else
        log_error "Lambda function not found in AWS"
        echo "Use '$0 create' to create the function"
    fi
    
    # Check if role exists
    if aws iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
        log_success "IAM role is active"
    else
        log_error "IAM role not found in AWS"
        echo "Use '$0 create' to create the role"
    fi
}

# Command: logs
cmd_logs() {
    local lines="${1:-50}"
    
    echo
    log_info "Fetching CloudWatch logs for Lambda function"
    echo "============================================="
    
    local region=$(get_region)
    
    # Check if function exists
    if ! aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_error "Lambda function '$FUNCTION_NAME' not found. Use '$0 create' first"
        exit 1
    fi
    
    log_info "Function: $FUNCTION_NAME"
    log_info "Region: $region"
    log_info "Fetching last $lines log entries"
    echo
    
    local log_group="/aws/lambda/$FUNCTION_NAME"
    
    # Check if log group exists
    if ! aws logs describe-log-groups --log-group-name-prefix "$log_group" --region "$region" --query 'logGroups[0]' --output text 2>/dev/null | grep -q "aws/lambda"; then
        log_warning "No log group found for function '$FUNCTION_NAME'"
        log_info "The function may not have been invoked yet, or logs may not be available"
        exit 0
    fi
    
    # Get recent log streams
    local log_streams=$(aws logs describe-log-streams \
        --log-group-name "$log_group" \
        --order-by LastEventTime \
        --descending \
        --max-items 5 \
        --region "$region" \
        --query 'logStreams[*].logStreamName' \
        --output text 2>/dev/null)
    
    if [[ -z "$log_streams" ]] || [[ "$log_streams" == "None" ]]; then
        log_warning "No log streams found for function '$FUNCTION_NAME'"
        log_info "The function may not have been invoked yet"
        exit 0
    fi
    
    log_info "Available log streams (showing logs from most recent):"
    echo
    
    # Fetch logs from recent streams
    local stream_count=0
    for stream in $log_streams; do
        if [[ $stream_count -ge 3 ]]; then  # Limit to 3 most recent streams
            break
        fi
        
        echo "----------------------------------------"
        log_info "Log Stream: $stream"
        echo "----------------------------------------"
        
        aws logs get-log-events \
            --log-group-name "$log_group" \
            --log-stream-name "$stream" \
            --region "$region" \
            --query "events[-$lines:].{Time:timestamp,Message:message}" \
            --output table 2>/dev/null || {
                log_warning "Could not retrieve logs from stream: $stream"
                continue
            }
        
        echo
        ((stream_count++))
    done
    
    if [[ $stream_count -eq 0 ]]; then
        log_warning "Could not retrieve any logs"
    else
        log_success "Logs fetched successfully!"
    fi
}

# Command: bench
cmd_bench() {
    local concurrent_invocations="${1:-100}"
    local payload="${2:-$TEST_PAYLOAD}"
    
    echo
    log_info "Benchmarking Lambda function with $concurrent_invocations concurrent invocations"
    echo "========================================================================"
    
    local region=$(get_region)
    
    # Check if function exists
    if ! aws lambda get-function --function-name "$FUNCTION_NAME" --region "$region" >/dev/null 2>&1; then
        log_error "Lambda function '$FUNCTION_NAME' not found. Use '$0 create' first"
        exit 1
    fi
    
    log_info "Function: $FUNCTION_NAME"
    log_info "Region: $region"
    log_info "Payload: $payload"
    log_info "Concurrent invocations: $concurrent_invocations"
    echo
    
    # Create temporary directory for results
    local temp_dir=$(mktemp -d)
    local start_time=$(date +%s.%N)
    
    log_info "Starting concurrent invocations..."
    
    # Function to invoke Lambda and capture result
    invoke_single() {
        local invocation_id=$1
        local result_file="$temp_dir/result_$invocation_id.json"
        local error_file="$temp_dir/error_$invocation_id.txt"
        local start_invoke=$(date +%s.%N)
        
        aws lambda invoke \
            --function-name "$FUNCTION_NAME" \
            --payload "$payload" \
            --region "$region" \
            --cli-binary-format raw-in-base64-out \
            "$result_file" > "$error_file" 2>&1
        
        local end_invoke=$(date +%s.%N)
        local duration=$(echo "$end_invoke - $start_invoke" | bc -l)
        
        # Check if invocation was successful
        if [[ $? -eq 0 ]] && [[ -f "$result_file" ]]; then
            echo "SUCCESS:$invocation_id:$duration" >> "$temp_dir/summary.txt"
        else
            echo "ERROR:$invocation_id:$duration" >> "$temp_dir/summary.txt"
        fi
    }
    
    # Export function and variables for parallel execution
    export -f invoke_single
    export FUNCTION_NAME
    export region
    export payload
    export temp_dir
    
    # Launch concurrent invocations
    for i in $(seq 1 $concurrent_invocations); do
        invoke_single $i &
    done
    
    # Wait for all background jobs to complete
    wait
    
    local end_time=$(date +%s.%N)
    local total_duration=$(echo "$end_time - $start_time" | bc -l)
    
    echo
    log_info "Analyzing results..."
    
    # Analyze results
    local success_count=0
    local error_count=0
    local total_invoke_time=0
    local min_time=999999
    local max_time=0
    
    if [[ -f "$temp_dir/summary.txt" ]]; then
        while IFS=':' read -r status id duration; do
            if [[ "$status" == "SUCCESS" ]]; then
                ((success_count++))
            else
                ((error_count++))
            fi
            
            total_invoke_time=$(echo "$total_invoke_time + $duration" | bc -l)
            
            # Update min/max times
            if (( $(echo "$duration < $min_time" | bc -l) )); then
                min_time=$duration
            fi
            if (( $(echo "$duration > $max_time" | bc -l) )); then
                max_time=$duration
            fi
        done < "$temp_dir/summary.txt"
    fi
    
    # Calculate statistics
    local avg_time=0
    if [[ $concurrent_invocations -gt 0 ]]; then
        avg_time=$(echo "scale=3; $total_invoke_time / $concurrent_invocations" | bc -l)
    fi
    
    local throughput=0
    if (( $(echo "$total_duration > 0" | bc -l) )); then
        throughput=$(echo "scale=2; $concurrent_invocations / $total_duration" | bc -l)
    fi
    
    # Display results
    echo
    log_success "Benchmark completed!"
    echo
    echo "=========================================="
    echo "BENCHMARK RESULTS"
    echo "=========================================="
    echo "Total Invocations:     $concurrent_invocations"
    echo "Successful:            $success_count"
    echo "Failed:                $error_count"
    echo "Success Rate:          $(echo "scale=2; $success_count * 100 / $concurrent_invocations" | bc -l)%"
    echo
    echo "TIMING"
    echo "----------------------------------------"
    printf "Total Duration:        %.3f seconds\n" "$total_duration"
    printf "Average Response Time: %.3f seconds\n" "$avg_time"
    printf "Min Response Time:     %.3f seconds\n" "$min_time"
    printf "Max Response Time:     %.3f seconds\n" "$max_time"
    printf "Throughput:            %.2f invocations/sec\n" "$throughput"
    echo "=========================================="
    
    # Show error details if any
    if [[ $error_count -gt 0 ]]; then
        echo
        log_warning "Errors detected. First few error details:"
        echo "----------------------------------------"
        local error_shown=0
        for error_file in "$temp_dir"/error_*.txt; do
            if [[ -f "$error_file" ]] && [[ $error_shown -lt 3 ]]; then
                echo "Error from $(basename "$error_file"):"
                head -5 "$error_file" 2>/dev/null || echo "Could not read error file"
                echo "----------------------------------------"
                ((error_shown++))
            fi
        done
    fi
    
    # Cleanup
    rm -rf "$temp_dir"
    
    echo
    log_info "Use '$0 logs' to check CloudWatch logs for detailed execution information"
}

# Command: help
cmd_help() {
    cat << EOF

Lambda Runtime Test Script
=========================

USAGE:
    $0 <command> [options]

COMMANDS:
    create [app_log_level] [system_log_level]
                      Create Lambda function and IAM role with JSON logging
                      Uses AWS CLI configured region or $DEFAULT_REGION as fallback
                      Default app_log_level: INFO (TRACE|DEBUG|INFO|WARN|ERROR|FATAL)
                      Default system_log_level: INFO (DEBUG|INFO|WARN)
    
    deploy            Update existing Lambda function with latest code
                      Rebuilds and redeploys without recreating resources
    
    invoke [payload]   Invoke the Lambda function with test payload
                      Default payload: $TEST_PAYLOAD
    
    bench [count] [payload]
                      Benchmark with concurrent invocations (requires 'bc' command)
                      Default count: 100, payload: $TEST_PAYLOAD
    
    logs [lines]      Fetch CloudWatch logs for the Lambda function
                      Default lines: 50
    
    cleanup           Delete Lambda function, IAM role and cleanup files
    
    status            Show current status of Lambda resources
    
    help              Show this help message

EXAMPLES:
    $0 create
    $0 create DEBUG INFO
    $0 deploy
    $0 invoke '{"message": "Custom test message"}'
    $0 bench
    $0 bench 50
    $0 bench 200 '{"test": "load testing"}'
    $0 logs
    $0 logs 100
    $0 cleanup

PREREQUISITES:
    - AWS CLI installed and configured (with default region set)
    - Go compiler installed
    - bc (basic calculator) for benchmark calculations
    - Appropriate AWS permissions for Lambda and IAM

REGION:
    The script uses the AWS CLI configured region (aws configure get region)
    or falls back to $DEFAULT_REGION if no region is configured.
    You can also set AWS_REGION environment variable.

RESOURCES:
    The script uses static resource names:
    - Function: $FUNCTION_NAME
    - IAM Role: $ROLE_NAME
    This eliminates the need for state files and makes resource management simpler.

EOF
}

# Main execution
main() {
    local command="$1"
    shift || true
    
    case "$command" in
        create)
            cmd_create "$@"
            ;;
        deploy)
            cmd_deploy "$@"
            ;;
        invoke)
            cmd_invoke "$@"
            ;;
        bench)
            cmd_bench "$@"
            ;;
        logs)
            cmd_logs "$@"
            ;;
        cleanup)
            cmd_cleanup "$@"
            ;;
        status)
            cmd_status "$@"
            ;;
        help|--help|-h)
            cmd_help
            ;;
        "")
            log_error "No command specified"
            echo
            cmd_help
            exit 1
            ;;
        *)
            log_error "Unknown command: $command"
            echo
            cmd_help
            exit 1
            ;;
    esac
}

# Run main function
main "$@"
