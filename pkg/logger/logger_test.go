package logger

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// captureOutput captures stdout and stderr output
func captureOutput(f func()) (string, string) {
	// Save original stdout and stderr
	originalStdout := os.Stdout
	originalStderr := os.Stderr

	// Create pipes
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()

	// Replace stdout and stderr
	os.Stdout = wOut
	os.Stderr = wErr

	// Run the function that produces output
	f()

	// Close writers and restore original stdout and stderr
	wOut.Close()
	wErr.Close()
	os.Stdout = originalStdout
	os.Stderr = originalStderr

	// Read captured output
	var bufOut, bufErr bytes.Buffer
	io.Copy(&bufOut, rOut)
	io.Copy(&bufErr, rErr)

	return bufOut.String(), bufErr.String()
}

func TestLogger_Info(t *testing.T) {
	// Test with text format
	logger := NewLogger(false)

	stdout, _ := captureOutput(func() {
		logger.Info("Test message")
	})

	assert.Contains(t, stdout, "[INFO]")
	assert.Contains(t, stdout, "Test message")

	// Test with dry-run
	logger = NewLogger(true)

	stdout, _ = captureOutput(func() {
		logger.Info("Dry run message")
	})

	assert.Contains(t, stdout, "[DRY-RUN]")
	assert.Contains(t, stdout, "Dry run message")

	// Test with JSON format
	logger = NewLogger(false)
	logger.SetFormat(JSONFormat)

	stdout, _ = captureOutput(func() {
		logger.Info("JSON message")
	})

	// Verify JSON structure
	var entry LogEntry
	err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &entry)
	assert.NoError(t, err)
	assert.Equal(t, InfoLevel, entry.Level)
	assert.Equal(t, "JSON message", entry.Message)
	assert.False(t, entry.DryRun)
}

func TestLogger_Success(t *testing.T) {
	logger := NewLogger(false)

	stdout, _ := captureOutput(func() {
		logger.Success("Success message")
	})

	assert.Contains(t, stdout, "[SUCCESS]")
	assert.Contains(t, stdout, "Success message")
}

func TestLogger_Warning(t *testing.T) {
	logger := NewLogger(false)

	stdout, _ := captureOutput(func() {
		logger.Warning("Warning message")
	})

	assert.Contains(t, stdout, "[WARNING]")
	assert.Contains(t, stdout, "Warning message")
}

func TestLogger_Error(t *testing.T) {
	logger := NewLogger(false)

	_, stderr := captureOutput(func() {
		logger.Error("Error message")
	})

	assert.Contains(t, stderr, "[ERROR]")
	assert.Contains(t, stderr, "Error message")

	// Test JSON error
	logger.SetFormat(JSONFormat)

	_, stderr = captureOutput(func() {
		logger.Error("JSON error")
	})

	// Verify JSON structure
	var entry LogEntry
	err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &entry)
	assert.NoError(t, err)
	assert.Equal(t, ErrorLevel, entry.Level)
	assert.Equal(t, "JSON error", entry.Message)
}

func TestLogger_FormatString(t *testing.T) {
	logger := NewLogger(false)

	stdout, _ := captureOutput(func() {
		logger.Info("Value: %d", 42)
	})

	assert.Contains(t, stdout, "Value: 42")

	stdout, _ = captureOutput(func() {
		logger.Success("Values: %s, %d", "test", 123)
	})

	assert.Contains(t, stdout, "Values: test, 123")
}

func TestLogger_SetFormat(t *testing.T) {
	logger := NewLogger(false)
	logger.SetFormat(JSONFormat)

	// Test with JSON format
	stdout, _ := captureOutput(func() {
		logger.Info("JSON format test")
	})

	// Should be valid JSON
	assert.Contains(t, stdout, "\"level\":\"INFO\"")
	assert.Contains(t, stdout, "\"message\":\"JSON format test\"")

	// Switch back to text format
	logger.SetFormat(TextFormat)

	stdout, _ = captureOutput(func() {
		logger.Info("Text format test")
	})

	// Should be plain text
	assert.Contains(t, stdout, "[INFO]")
	assert.Contains(t, stdout, "Text format test")
}
