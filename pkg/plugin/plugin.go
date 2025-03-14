package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/k8s-rollout-restart/pkg/logger"
	"k8s.io/client-go/kubernetes"
)

// ResourceType represents the type of Kubernetes resource
type ResourceType string

// Operation defines the type of operation to be performed
type Operation string

const (
	// OperationRestart represents a restart operation
	OperationRestart Operation = "restart"
	// OperationCordon represents a cordon operation
	OperationCordon Operation = "cordon"
	// OperationUncordon represents an uncordon operation
	OperationUncordon Operation = "uncordon"
)

// PluginOptions contains options for plugin execution
type PluginOptions struct {
	DryRun    bool
	Parallel  int
	Timeout   int
	Namespace string
}

// ResourceHandler is the interface implemented by resource handlers
type ResourceHandler interface {
	// GetResourceType returns the resource type handled by this plugin
	GetResourceType() ResourceType

	// SupportedOperations returns the operations supported by this plugin
	SupportedOperations() []Operation

	// Execute executes the given operation on the resource
	Execute(ctx context.Context, op Operation, options PluginOptions) error
}

// PluginManager manages the registration and execution of plugins
type PluginManager struct {
	handlers map[ResourceType]ResourceHandler
	client   *kubernetes.Clientset
	log      *logger.Logger
	mu       sync.RWMutex
}

// NewPluginManager creates a new plugin manager
func NewPluginManager(client *kubernetes.Clientset, log *logger.Logger) *PluginManager {
	return &PluginManager{
		handlers: make(map[ResourceType]ResourceHandler),
		client:   client,
		log:      log,
	}
}

// RegisterHandler registers a new resource handler
func (p *PluginManager) RegisterHandler(handler ResourceHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()

	resourceType := handler.GetResourceType()
	p.handlers[resourceType] = handler
	p.log.Info("Registered plugin for resource type: %s", resourceType)
}

// GetHandler returns the handler for the given resource type
func (p *PluginManager) GetHandler(resourceType ResourceType) (ResourceHandler, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	handler, exists := p.handlers[resourceType]
	if !exists {
		return nil, fmt.Errorf("no handler registered for resource type %s", resourceType)
	}

	return handler, nil
}

// ExecuteOperation executes the given operation on the given resource type
func (p *PluginManager) ExecuteOperation(
	ctx context.Context,
	resourceType ResourceType,
	op Operation,
	options PluginOptions,
) error {
	handler, err := p.GetHandler(resourceType)
	if err != nil {
		return err
	}

	// Check if the operation is supported
	supported := false
	for _, supportedOp := range handler.SupportedOperations() {
		if supportedOp == op {
			supported = true
			break
		}
	}

	if !supported {
		return fmt.Errorf("operation %s not supported by handler for resource type %s", op, resourceType)
	}

	// Execute the operation
	p.log.Info("Executing %s operation on %s resources", op, resourceType)
	return handler.Execute(ctx, op, options)
}
