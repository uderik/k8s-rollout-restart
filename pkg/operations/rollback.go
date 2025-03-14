package operations

import (
	"context"
	"fmt"
	"sync"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
)

// RollbackOperation represents an operation that can be rolled back
type RollbackOperation struct {
	Description string
	RollbackFn  func(ctx context.Context) error
}

// RollbackManager manages rollback operations
type RollbackManager struct {
	operations []RollbackOperation
	lock       sync.Mutex
	log        *logger.Logger
}

// NewRollbackManager creates a new RollbackManager
func NewRollbackManager(log *logger.Logger) *RollbackManager {
	return &RollbackManager{
		operations: []RollbackOperation{},
		lock:       sync.Mutex{},
		log:        log,
	}
}

// RegisterRollback registers a rollback operation
func (r *RollbackManager) RegisterRollback(description string, rollbackFn func(ctx context.Context) error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.operations = append(r.operations, RollbackOperation{
		Description: description,
		RollbackFn:  rollbackFn,
	})

	r.log.Info("Registered rollback operation: %s", description)
}

// RegisterConditional registers a rollback operation with a condition
func (r *RollbackManager) RegisterConditional(condition bool, description string, rollbackFn func(ctx context.Context) error) {
	if condition {
		r.RegisterRollback(description, rollbackFn)
	}
}

// Clear clears all registered rollback operations
func (r *RollbackManager) Clear() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.operations = []RollbackOperation{}
	r.log.Info("Cleared all rollback operations")
}

// Rollback performs all registered rollback operations in reverse order
func (r *RollbackManager) Rollback(ctx context.Context) error {
	r.lock.Lock()
	defer r.lock.Unlock()

	if len(r.operations) == 0 {
		return nil
	}

	r.log.Info("Starting rollback of %d operations", len(r.operations))
	var lastErr error

	for i := len(r.operations) - 1; i >= 0; i-- {
		op := r.operations[i]
		r.log.Info("Rolling back operation: %s", op.Description)

		if err := op.RollbackFn(ctx); err != nil {
			// Use constant format string with variable parameters
			r.log.Error("Failed to roll back operation %s: %v", op.Description, err)
			lastErr = fmt.Errorf("failed to roll back operation %s: %w", op.Description, err)

			// Continue with other rollbacks even if one fails
			continue
		}

		r.log.Success("Successfully rolled back operation: %s", op.Description)
	}

	if lastErr != nil {
		return lastErr
	}

	r.log.Success("Successfully completed all rollback operations")
	r.operations = []RollbackOperation{} // Clear operations after successful rollback
	return nil
}
