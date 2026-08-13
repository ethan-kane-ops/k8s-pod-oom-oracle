package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "k8s-pod-oom-oracle",
	Short: "Kubernetes system daemon and CLI for low-level process-aware OOM diagnostics using cgroups and eBPF tracing",
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
