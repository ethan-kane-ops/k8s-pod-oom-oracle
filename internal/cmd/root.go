// Package cmd assembles the cobra command tree for the oom-oracle binary.
package cmd

import (
	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version"
)

const longDescription = `OOM Oracle predicts, detects, and explains Kubernetes OOM kills.

Kubernetes reports only a generic OOMKilled status with exit code 137. It cannot
tell you which process inside a multi-process container died, or what the memory
curve looked like on the way there. OOM Oracle watches cgroup memory controllers
and (where the kernel supports it) traces OOM kills with eBPF, then correlates
each kill back to its pod, container, and victim process.`

// NewRootCmd builds a fresh command tree. Constructing a new tree per call keeps
// flag state out of package globals, so tests can run in parallel.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "oom-oracle",
		Short: "Process-aware OOM diagnostics for Kubernetes",
		Long:  longDescription,
		// Errors are printed once by Execute; usage on a runtime error is noise.
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Get().Version,
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")

	root.AddCommand(newVersionCmd())

	return root
}

// Execute runs the root command. The caller owns exit-code handling.
func Execute() error {
	return NewRootCmd().Execute()
}
