// Package main is the entry point for k8s-rollout-restart CLI application.
package main

import (
	"fmt"
	"os"

	"github.com/uderik/k8s-rollout-restart/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
