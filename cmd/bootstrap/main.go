package main

import (
	"context"
	"errors"
	"os"

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
	// Access any environment variable via the All map
	if reqCtx, ok := runtime.FromContext(ctx); ok {
		h.log.Info(ctx, "request context", "aws_request_id", reqCtx.AwsRequestID)
	}

	return Output{Input: e.Input, Output: e.Input}, nil
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
