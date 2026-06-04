// Package logger provides structured logging functionality for k8s-rollout-restart.
package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fatih/color"
)

var (
	infoColor    = color.New(color.FgCyan)
	successColor = color.New(color.FgGreen)
	warningColor = color.New(color.FgYellow)
	errorColor   = color.New(color.FgRed)
)

// LogFormat defines the format of logs
type LogFormat string

const (
	// TextFormat is human-readable colored text output
	TextFormat LogFormat = "text"
	// JSONFormat is machine-parseable JSON output
	JSONFormat LogFormat = "json"
)

// LogLevel defines the severity of log messages
type LogLevel string

const (
	// InfoLevel is for informational messages
	InfoLevel LogLevel = "INFO"
	// SuccessLevel is for success messages
	SuccessLevel LogLevel = "SUCCESS"
	// WarningLevel is for warning messages
	WarningLevel LogLevel = "WARNING"
	// ErrorLevel is for error messages
	ErrorLevel LogLevel = "ERROR"
)

// LogEntry represents a structured log entry
type LogEntry struct {
	Timestamp string   `json:"timestamp"`
	Level     LogLevel `json:"level"`
	Message   string   `json:"message"`
	DryRun    bool     `json:"dry_run,omitempty"`
}

// Logger provides formatted logging functionality.
// It is safe for concurrent use by multiple goroutines: a single Logger
// instance is shared across the parallel restart workers, so all output is
// serialized through mu to avoid interleaved/garbled lines (color escape
// sequences emit several writes per message).
type Logger struct {
	mu     sync.Mutex
	dryRun bool
	format LogFormat
}

// NewLogger creates a new Logger instance
func NewLogger(dryRun bool) *Logger {
	return &Logger{
		dryRun: dryRun,
		format: TextFormat,
	}
}

// SetFormat sets the log output format
func (l *Logger) SetFormat(format LogFormat) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.format = format
}

// Info logs an informational message
func (l *Logger) Info(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == JSONFormat {
		l.logJSON(InfoLevel, message)
	} else {
		prefix := "[INFO]"
		if l.dryRun {
			prefix = "[DRY-RUN]"
		}
		_, _ = infoColor.Fprintf(os.Stdout, "%s %s\n", prefix, message)
	}
}

// Success logs a success message
func (l *Logger) Success(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == JSONFormat {
		l.logJSON(SuccessLevel, message)
	} else {
		prefix := "[SUCCESS]"
		if l.dryRun {
			prefix = "[DRY-RUN]"
		}
		_, _ = successColor.Fprintf(os.Stdout, "%s %s\n", prefix, message)
	}
}

// Warning logs a warning message
func (l *Logger) Warning(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == JSONFormat {
		l.logJSON(WarningLevel, message)
	} else {
		prefix := "[WARNING]"
		if l.dryRun {
			prefix = "[DRY-RUN]"
		}
		_, _ = warningColor.Fprintf(os.Stdout, "%s %s\n", prefix, message)
	}
}

// Error logs an error message
func (l *Logger) Error(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.format == JSONFormat {
		l.logJSON(ErrorLevel, message)
	} else {
		prefix := "[ERROR]"
		if l.dryRun {
			prefix = "[DRY-RUN]"
		}
		_, _ = errorColor.Fprintf(os.Stderr, "%s %s\n", prefix, message)
	}
}

// logJSON outputs a log entry in JSON format
func (l *Logger) logJSON(level LogLevel, message string) {
	entry := LogEntry{
		Timestamp: time.Now().Format(time.RFC3339),
		Level:     level,
		Message:   message,
	}

	if l.dryRun {
		entry.DryRun = true
	}

	jsonData, err := json.Marshal(entry)
	if err != nil {
		// Fallback to text if JSON marshalling fails
		fmt.Fprintf(os.Stderr, "[ERROR] Failed to marshal log entry: %v\n", err)
		return
	}

	// Write to stdout for all levels except ERROR
	if level == ErrorLevel {
		fmt.Fprintln(os.Stderr, string(jsonData))
	} else {
		_, _ = fmt.Fprintln(os.Stdout, string(jsonData))
	}
}
