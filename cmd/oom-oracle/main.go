// Command oom-oracle is the CLI and node daemon for process-aware Kubernetes
// OOM diagnostics.
package main

import (
	"fmt"
	"os"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
