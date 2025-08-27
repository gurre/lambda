package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gurre/lambda/log"
	"github.com/gurre/lambda/runtime"
)

// Event is the payload shape expected by this Lambda.
type Input struct {
	Input string `json:"input"`
}

type Output struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// Handler implements runtime.Handler[Input, Output] directly.
type Handler struct {
	log log.Logger
	// cfg aws.Config
	// writer *s3streamer.CompressedS3Writer
}

// cpuSink prevents the compiler from eliminating synthetic CPU work.
var cpuSink uint64

// NewHandler creates a new Handler with default configuration.
func NewHandler() *Handler {
	return &Handler{
		// cfg: aws.Config{},
		log: log.New(log.LevelInfo, os.Stdout),
	}
}

func (h *Handler) ColdStart(ctx context.Context) error {
	// Example on how to stream logs to s3, there will still be some runtime report logs sent to cloudwatch.
	// Create our own log stream writer. By using the same log group and stream name we can later correlate logs.
	// key := fmt.Sprintf("%s/%s/%s.jsonl.gz", os.Getenv("S3_LOG_BUCKET"), runtime.LogGroupName, runtime.LogStreamName)
	// writer, err := s3streamer.NewCompressedS3Writer(context.Background(), s3.NewFromConfig(h.cfg), os.Getenv("S3_LOG_BUCKET"), key, 5*1024*1024, s3streamer.Gzip)
	// if err != nil {
	// 	return err
	// }

	// h.writer = writer
	// h.log = log.New(log.LevelInfo, writer)

	return nil
}

func (h *Handler) Validate(ctx context.Context, e Input) error {
	if e.Input == "" {
		return errors.New("input is required")
	}
	return nil
}

func (h *Handler) Handler(ctx context.Context, e Input) (Output, error) {
	// Optional synthetic workload controls via input string:
	//  - "sleep:<duration>" (e.g., sleep:50ms)
	//  - "cpu:<duration>" busy loop (e.g., cpu:50ms)
	if strings.HasPrefix(e.Input, "sleep:") {
		d, err := time.ParseDuration(strings.TrimPrefix(e.Input, "sleep:"))
		if err == nil {
			time.Sleep(d)
		}
	}
	if strings.HasPrefix(e.Input, "cpu:") {
		d, err := time.ParseDuration(strings.TrimPrefix(e.Input, "cpu:"))
		if err == nil {
			end := time.Now().Add(d)
			var x uint64 = 1469598103934665603
			for time.Now().Before(end) {
				// Simple busy work
				x ^= 1099511628211
				x *= 16777619
			}
			// Publish result to a global sink to prevent optimization.
			atomic.AddUint64(&cpuSink, x|1)
		}
	}

	return Output{Input: e.Input, Output: fmt.Sprintf("processed:%s", e.Input)}, nil
}

// Shutdown implements the Handler interface.
func (h *Handler) Shutdown(ctx context.Context) error {
	// Close completes the multipart upload.
	// return h.writer.Close()
	return nil
}

func main() {
	runtime.Start(NewHandler())
}
