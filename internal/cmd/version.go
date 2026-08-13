package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version"
)

const (
	formatText = "text"
	formatJSON = "json"
)

func newVersionCmd() *cobra.Command {
	var format string

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build metadata",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), format)
		},
	}
	cmd.Flags().StringVarP(&format, "output", "o", formatText, "output format: text|json")

	return cmd
}

func writeVersion(w io.Writer, format string) error {
	info := version.Get()

	var payload []byte
	switch format {
	case formatText:
		payload = []byte(info.String())
	case formatJSON:
		encoded, err := info.JSON()
		if err != nil {
			return err
		}
		payload = encoded
	default:
		return fmt.Errorf("unknown output format %q: want %s or %s", format, formatText, formatJSON)
	}

	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("writing version output: %w", err)
	}
	return nil
}
