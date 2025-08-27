package log

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
)

// Global json-iterator instance for performance with optimized float handling
var JsoniterAPI = jsoniter.Config{
	EscapeHTML:                    true,
	SortMapKeys:                   true,
	ValidateJsonRawMessage:        true,
	UseNumber:                     false, // Use float64 for better performance
	MarshalFloatWith6Digits:       true,  // will lose precession
	ObjectFieldMustBeSimpleString: true,  // do not unescape object field
}.Froze()

// Pool for reusing map[string]any to reduce allocations
var entryPool = sync.Pool{
	New: func() interface{} {
		// Pre-allocate with larger capacity for typical Lambda log entries
		return make(map[string]any, 16)
	},
}

// Pool for reusing string builders for timestamp formatting
var timestampPool = sync.Pool{
	New: func() interface{} {
		builder := &strings.Builder{}
		builder.Grow(32) // Pre-allocate for RFC3339Nano timestamp
		return builder
	},
}

// Pre-computed level strings to avoid allocations
var levelStrings = [...]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

type jsonLogger struct {
	level  Level
	out    io.Writer
	mu     sync.Mutex
	fields map[string]any
}

func New(level Level, w io.Writer) Logger {
	if w == nil {
		w = io.Discard
	}
	return &jsonLogger{level: level, out: w, fields: nil} // Start with nil to save memory
}

func (l *jsonLogger) With(args ...any) Logger {
	newFields := parseArgs(args...)
	if newFields == nil && len(l.fields) == 0 {
		// No fields to add, return a logger with nil fields to save memory
		return &jsonLogger{level: l.level, out: l.out, fields: nil}
	}

	// Pre-calculate capacity for the new fields map
	totalCap := len(l.fields) + len(newFields)
	childFields := make(map[string]any, totalCap)

	// Copy existing fields
	for k, v := range l.fields {
		childFields[k] = v
	}

	// Add new fields
	for k, v := range newFields {
		childFields[k] = v
	}

	return &jsonLogger{level: l.level, out: l.out, fields: childFields}
}

func (l *jsonLogger) WithError(err error) Logger {
	if err == nil {
		return l
	}
	return l.With("error", err.Error())
}

func (l *jsonLogger) log(ctx context.Context, level Level, msg string, fields map[string]any) {
	// Check if we should log at this level
	if level < l.level {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Get reusable entry map from pool
	entry := entryPool.Get().(map[string]any)
	defer func() {
		// Clear the map and return to pool
		for k := range entry {
			delete(entry, k)
		}
		entryPool.Put(entry)
	}()

	// Optimize timestamp generation with pooled string builder
	timestampBuilder := timestampPool.Get().(*strings.Builder)
	timestampBuilder.Reset()
	now := time.Now().UTC()
	// Use faster timestamp formatting
	timestampBuilder.WriteString(now.Format("2006-01-02T15:04:05.000000000Z"))
	timestamp := timestampBuilder.String()
	timestampPool.Put(timestampBuilder)

	// Add core fields directly to avoid allocations
	entry["timestamp"] = timestamp
	entry["level"] = levelToString(level)
	entry["message"] = msg

	// Add persistent fields (only if they exist)
	if len(l.fields) > 0 {
		for k, v := range l.fields {
			entry[k] = v
		}
	}

	// Add context fields (only if they exist)
	if len(fields) > 0 {
		for k, v := range fields {
			entry[k] = v
		}
	}

	// Use the stream for marshaling to avoid allocations
	stream := JsoniterAPI.BorrowStream(l.out)
	defer JsoniterAPI.ReturnStream(stream)

	// Marshal the entry to JSON
	stream.WriteVal(entry)

	// Ensure the stream is flushed to the underlying writer
	if stream.Error != nil {
		// Fallback to a simple error message if marshaling fails
		// Use pooled timestamp builder for error case too
		errorTimestampBuilder := timestampPool.Get().(*strings.Builder)
		errorTimestampBuilder.Reset()
		errorTimestampBuilder.WriteString(time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"))
		errorTimestamp := errorTimestampBuilder.String()
		timestampPool.Put(errorTimestampBuilder)

		fallbackMsg := `{"level":"ERROR","message":"failed to marshal log entry","timestamp":"` + errorTimestamp + `"}`
		_, _ = l.out.Write(unsafeStringToBytes(fallbackMsg))
	} else {
		// Flush the stream to ensure all data is written
		stream.Flush()
	}

	// Add newline
	_, _ = l.out.Write([]byte{'\n'})
}

// Unsafe string-to-bytes conversion to avoid allocation when we know the string is immutable
func unsafeStringToBytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func (l *jsonLogger) Debug(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelDebug, msg, parseArgs(args...))
}

func (l *jsonLogger) Info(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelInfo, msg, parseArgs(args...))
}

func (l *jsonLogger) Warn(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelWarn, msg, parseArgs(args...))
}

func (l *jsonLogger) Error(ctx context.Context, msg string, args ...any) {
	l.log(ctx, LevelError, msg, parseArgs(args...))
}

// levelToString converts a Level to its string representation using pre-computed strings.
func levelToString(level Level) string {
	if level >= 0 && int(level) < len(levelStrings) {
		return levelStrings[level]
	}
	return "INFO"
}

// parseArgsOptimized optimizes parseArgs by pre-calculating capacity and using pooled maps.
func parseArgs(args ...any) map[string]any {
	if len(args) == 0 {
		return nil // Return nil for empty to avoid allocation
	}

	if len(args) == 1 {
		if m, ok := args[0].(map[string]any); ok {
			return m // Return original map directly to avoid copying
		}
	}

	// Pre-calculate capacity for key-value pairs with better sizing
	capacity := (len(args) + 1) / 2 // Round up for odd number of args
	if capacity < 4 {
		capacity = 4 // Minimum capacity to reduce map reallocation
	}

	out := make(map[string]any, capacity)
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			out[k] = args[i+1]
		}
	}
	return out
}
