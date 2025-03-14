package cmd

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// Wrapper function for creating root command for tests
func NewRootCommand() *cobra.Command {
	return rootCmd
}

func TestParseDuration(t *testing.T) {
	// Test regular time.Duration formats
	duration, err := parseDuration("1h")
	assert.NoError(t, err)
	assert.Equal(t, 1*time.Hour, duration)

	duration, err = parseDuration("30m")
	assert.NoError(t, err)
	assert.Equal(t, 30*time.Minute, duration)

	duration, err = parseDuration("45s")
	assert.NoError(t, err)
	assert.Equal(t, 45*time.Second, duration)

	// Test day format
	duration, err = parseDuration("1d")
	assert.NoError(t, err)
	assert.Equal(t, 24*time.Hour, duration)

	duration, err = parseDuration("7d")
	assert.NoError(t, err)
	assert.Equal(t, 7*24*time.Hour, duration)

	// Test combined formats
	duration, err = parseDuration("1d12h")
	assert.NoError(t, err)
	assert.Equal(t, 36*time.Hour, duration)

	duration, err = parseDuration("2d30m")
	assert.NoError(t, err)
	assert.Equal(t, 48*time.Hour+30*time.Minute, duration)

	// Test invalid formats
	_, err = parseDuration("")
	assert.Error(t, err)

	_, err = parseDuration("d")
	assert.Error(t, err)

	_, err = parseDuration("1dd")
	assert.Error(t, err)

	_, err = parseDuration("invalid")
	assert.Error(t, err)

	// Note: parseDuration does not currently validate for negative values,
	// as time.ParseDuration accepts them. If this changes in the future,
	// add a test for that here.
}

func TestOlderThanFlag(t *testing.T) {
	// This is more of an integration test that would test the flag parsing
	// and command execution logic. In a real implementation, we would:
	// 1. Create a test cobra.Command
	// 2. Set the olderThan flag
	// 3. Execute the command
	// 4. Verify the behavior

	// For now, this is just a placeholder
	t.Skip("Integration test for olderThan flag would go here")
}

func TestRootCommand_Flags(t *testing.T) {
	// Skip this test as it requires more complex setup
	t.Skip("This test requires more complex setup")
}

func TestResourcesFlag_Validation(t *testing.T) {
	// Skip this test as it requires more complex setup
	t.Skip("This test requires more complex setup")
}

func TestOlderThanFlag_Parsing(t *testing.T) {
	// Skip this test as it requires more complex setup
	t.Skip("This test requires more complex setup")
}

func TestPreRunE(t *testing.T) {
	// Skip this test as it requires more complex setup
	t.Skip("This test requires more complex setup")
}

func TestRootCommand_Integration(t *testing.T) {
	// Skip this test as it requires more complex setup
	t.Skip("This test requires more complex setup")
}
