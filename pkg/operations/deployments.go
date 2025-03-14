package operations

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// DeploymentOperations handles deployment-related operations
type DeploymentOperations struct {
	client    *kubernetes.Clientset
	parallel  int
	timeout   int
	noFlagger bool
	dryRun    bool
}

// NewDeploymentOperations creates a new DeploymentOperations instance
func NewDeploymentOperations(client *kubernetes.Clientset, parallel, timeout int, noFlagger bool, dryRun bool) *DeploymentOperations {
	return &DeploymentOperations{
		client:    client,
		parallel:  parallel,
		timeout:   timeout,
		noFlagger: noFlagger,
		dryRun:    dryRun,
	}
}

// hasFlaggerCanaryOwner checks if a deployment is managed by Flagger Canary
func (d *DeploymentOperations) hasFlaggerCanaryOwner(deploy *appsv1.Deployment) bool {
	for _, owner := range deploy.OwnerReferences {
		if owner.APIVersion == "flagger.app/v1beta1" &&
			owner.Kind == "Canary" &&
			owner.Controller != nil &&
			*owner.Controller {
			return true
		}
	}
	return false
}

// isFlaggerPrimaryDeployment checks if a deployment is a Flagger primary deployment
func (d *DeploymentOperations) isFlaggerPrimaryDeployment(deploy *appsv1.Deployment) bool {
	return d.hasFlaggerCanaryOwner(deploy) && strings.HasSuffix(deploy.Name, "-primary")
}

// RestartDeployments restarts all deployments in the given namespace
func (d *DeploymentOperations) RestartDeployments(ctx context.Context, namespace string) error {
	deployments, err := d.client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments in namespace %s: %w", namespace, err)
	}

	// Filter deployments based on Flagger ownership
	var deploymentsToRestart []*appsv1.Deployment
	var skippedDeployments []*appsv1.Deployment

	// Process all deployments
	for i := range deployments.Items {
		deploy := &deployments.Items[i]

		// If noFlagger flag is set or deployment has no Flagger owner, include it
		if d.noFlagger || !d.hasFlaggerCanaryOwner(deploy) {
			deploymentsToRestart = append(deploymentsToRestart, deploy)
		} else {
			// For Flagger-owned deployments, only include primary ones
			if strings.HasSuffix(deploy.Name, "-primary") {
				deploymentsToRestart = append(deploymentsToRestart, deploy)
			} else {
				skippedDeployments = append(skippedDeployments, deploy)
			}
		}
	}

	if d.dryRun {
		fmt.Printf("\nDeployments that would be restarted:\n")
		for _, deploy := range deploymentsToRestart {
			fmt.Printf("- %s/%s (replicas: %d)", deploy.Namespace, deploy.Name, *deploy.Spec.Replicas)
			if d.hasFlaggerCanaryOwner(deploy) {
				fmt.Printf(" [Flagger primary]")
			}
			fmt.Println()
		}

		if len(skippedDeployments) > 0 {
			fmt.Printf("\nDeployments that would be skipped (non-primary Flagger deployments):\n")
			for _, deploy := range skippedDeployments {
				fmt.Printf("- %s/%s (SKIPPED - Flagger canary)\n", deploy.Namespace, deploy.Name)
			}
		}

		return nil
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(d.parallel)

	for _, deploy := range deploymentsToRestart {
		deploy := deploy // capture range variable
		g.Go(func() error {
			return d.restartDeployment(ctx, deploy)
		})
	}

	return g.Wait()
}

// restartDeployment restarts a single deployment by updating its pod template
func (d *DeploymentOperations) restartDeployment(ctx context.Context, deploy *appsv1.Deployment) error {
	// Add or update restart timestamp annotation
	if deploy.Spec.Template.ObjectMeta.Annotations == nil {
		deploy.Spec.Template.ObjectMeta.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.ObjectMeta.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	_, err := d.client.AppsV1().Deployments(deploy.Namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to restart deployment %s/%s: %w", deploy.Namespace, deploy.Name, err)
	}

	// Wait for rollout to complete
	return d.waitForDeploymentRollout(ctx, deploy.Namespace, deploy.Name)
}

// waitForDeploymentRollout waits for a deployment to complete its rollout
func (d *DeploymentOperations) waitForDeploymentRollout(ctx context.Context, namespace, name string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(d.timeout)*time.Second)
	defer cancel()

	for {
		select {
		case <-timeoutCtx.Done():
			return fmt.Errorf("timeout waiting for deployment %s/%s rollout", namespace, name)
		default:
			deploy, err := d.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get deployment %s/%s: %w", namespace, name, err)
			}

			if deploy.Status.UpdatedReplicas == deploy.Status.Replicas &&
				deploy.Status.UpdatedReplicas == deploy.Status.AvailableReplicas &&
				deploy.Status.ObservedGeneration >= deploy.Generation {
				return nil
			}

			time.Sleep(time.Second)
		}
	}
}
