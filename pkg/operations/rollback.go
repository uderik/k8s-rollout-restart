package operations

import (
	"context"
	"fmt"
	"sync"

	"github.com/k8s-rollout-restart/pkg/logger"
)

// RollbackOperation represents an operation that can be rolled back
type RollbackOperation struct {
	Description string
	RollbackFn  func(context.Context) error
}

// RollbackManager manages rollback operations
type RollbackManager struct {
	operations []RollbackOperation
	mu         sync.Mutex
	log        *logger.Logger
}

// NewRollbackManager creates a new RollbackManager
func NewRollbackManager(log *logger.Logger) *RollbackManager {
	return &RollbackManager{
		operations: make([]RollbackOperation, 0),
		log:        log,
	}
}

// AddRollbackOperation adds a rollback operation to the manager
func (r *RollbackManager) AddRollbackOperation(description string, fn func(context.Context) error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.operations = append(r.operations, RollbackOperation{
		Description: description,
		RollbackFn:  fn,
	})

	r.log.Info("Registered rollback operation: %s", description)
}

// Rollback executes all rollback operations in reverse order
func (r *RollbackManager) Rollback(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.operations) == 0 {
		r.log.Info("No rollback operations to perform")
		return nil
	}

	r.log.Warning("Starting rollback of %d operations", len(r.operations))

	var lastErr error

	// Execute operations in reverse order (last in, first out)
	for i := len(r.operations) - 1; i >= 0; i-- {
		op := r.operations[i]
		r.log.Info("Rolling back operation: %s", op.Description)

		if err := op.RollbackFn(ctx); err != nil {
			errMsg := fmt.Sprintf("Failed to roll back operation %s: %v", op.Description, err)
			r.log.Error(errMsg)
			lastErr = fmt.Errorf(errMsg)

			// Continue with other rollbacks even if one fails
			continue
		}

		r.log.Success("Successfully rolled back operation: %s", op.Description)
	}

	if lastErr != nil {
		return fmt.Errorf("rollback completed with errors: %w", lastErr)
	}

	r.log.Success("Rollback completed successfully")
	return nil
}

// Clear clears all registered rollback operations
func (r *RollbackManager) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.operations = make([]RollbackOperation, 0)
}
