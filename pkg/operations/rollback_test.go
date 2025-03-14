package operations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/uderik/k8s-rollout-restart/pkg/logger"
)

func TestRollbackManager(t *testing.T) {
	// Create a new rollback manager
	log := logger.NewLogger(true)
	mgr := NewRollbackManager(log)

	operationExecuted := false

	// Test registering a rollback operation
	mgr.RegisterRollback("Test Operation", func(ctx context.Context) error {
		operationExecuted = true
		return nil
	})

	// Test successful rollback execution
	err := mgr.Rollback(context.Background())
	assert.NoError(t, err, "Rollback should not return an error")
	assert.True(t, operationExecuted, "Operation should have been executed")

	// Reset and test Clear method
	operationExecuted = false
	mgr.RegisterRollback("Test Operation 2", func(ctx context.Context) error {
		operationExecuted = true
		return nil
	})

	mgr.Clear()

	// After clear, a rollback should not execute the operation
	err = mgr.Rollback(context.Background())
	assert.NoError(t, err, "Rollback should not return an error after clear")
	assert.False(t, operationExecuted, "Operation should not have been executed after clear")

	// Test rollback with error
	errorExecuted := false
	mgr.RegisterRollback("Error Operation", func(ctx context.Context) error {
		errorExecuted = true
		return errors.New("simulated error")
	})

	err = mgr.Rollback(context.Background())
	assert.Error(t, err, "Rollback should return an error")
	assert.Contains(t, err.Error(), "simulated error", "Error message should contain the original error")
	assert.True(t, errorExecuted, "Error operation should have been executed")

	// Test multiple operations with mixed results
	operation1Executed := false
	operation2Executed := false
	errorOperationExecuted := false

	mgr.RegisterRollback("Success Operation 1", func(ctx context.Context) error {
		operation1Executed = true
		return nil
	})
	mgr.RegisterRollback("Error Operation", func(ctx context.Context) error {
		errorOperationExecuted = true
		return errors.New("another error")
	})
	mgr.RegisterRollback("Success Operation 2", func(ctx context.Context) error {
		operation2Executed = true
		return nil
	})

	// All operations should be executed, even if some fail
	err = mgr.Rollback(context.Background())
	assert.Error(t, err, "Rollback should return an error when any operation fails")

	// Print the error for debugging
	t.Logf("Error returned: %v", err)

	// The error message should contain the operation name and error message
	assert.True(t, strings.Contains(err.Error(), "Error Operation"), "Error message should contain the operation name")
	assert.True(t, strings.Contains(err.Error(), "simulated error"), "Error should contain the error message")

	// Verify all operations were executed
	assert.True(t, operation1Executed, "Operation 1 should have been executed")
	assert.True(t, errorOperationExecuted, "Error operation should have been executed")
	assert.True(t, operation2Executed, "Operation 2 should have been executed")
}

func TestRollbackManager_Empty(t *testing.T) {
	// Create a new rollback manager
	log := logger.NewLogger(true)
	mgr := NewRollbackManager(log)

	// Calling rollback on an empty manager should not error
	err := mgr.Rollback(context.Background())
	assert.NoError(t, err, "Rollback on empty manager should not error")
}

func TestRollbackManager_ContextCancellation(t *testing.T) {
	// Create a new rollback manager
	log := logger.NewLogger(true)
	mgr := NewRollbackManager(log)

	operationExecuted := false

	// Register an operation
	mgr.RegisterRollback("Test Operation", func(ctx context.Context) error {
		// Check if context is cancelled
		select {
		case <-ctx.Done():
			operationExecuted = true
			return ctx.Err()
		default:
			return nil
		}
	})

	// Create a cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Rollback should return an error that includes the context cancellation
	err := mgr.Rollback(ctx)
	assert.Error(t, err, "Rollback with cancelled context should error")
	assert.Contains(t, err.Error(), "context canceled", "Error should mention context cancellation")
	assert.True(t, operationExecuted, "Operation should have been executed with cancelled context")
}
