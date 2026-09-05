package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/tui"
)

// minWatchInterval floors the refresh rate.
//
// Each refresh is two HTTP requests against a daemon on a node that is, by the
// time anyone is watching this, probably under memory pressure. A tighter loop
// buys no information: the daemon samples once a second, so anything faster
// re-renders the same numbers.
const minWatchInterval = 500 * time.Millisecond

type watchConfig struct {
	daemonAddr string
	interval   time.Duration
}

func newWatchCmd() *cobra.Command {
	cfg := watchConfig{}

	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Live terminal dashboard of OOM kills on a node",
		Long: `Watch a running daemon and browse the kills it records.

The left pane lists post-mortems newest first; the right pane renders the
selected one, using the same renderer as ` + "`oom-oracle inspect`" + `. The header
states which detector is active, because an inferred victim and a traced one
look identical in every field but a boolean.

This reads the daemon's HTTP API and needs no privileges of its own. Port-forward
to a node agent and point --daemon at it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWatch(cmd.Context(), cmd.OutOrStdout(), cmd.InOrStdin(), cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.daemonAddr, "daemon", "http://127.0.0.1:9090", "base URL of the oom-oracle daemon")
	flags.DurationVar(&cfg.interval, "interval", 2*time.Second, "how often to refresh")

	return cmd
}

func runWatch(ctx context.Context, out io.Writer, in io.Reader, cfg watchConfig) error {
	if cfg.interval < minWatchInterval {
		return fmt.Errorf("--interval %s is below the %s minimum", cfg.interval, minWatchInterval)
	}

	model := tui.New(tui.Options{
		Client:   &tui.HTTPClient{Addr: cfg.daemonAddr},
		Interval: cfg.interval,
		Addr:     cfg.daemonAddr,
	})

	// The output and input are taken from the command rather than assumed to be
	// the process's own, so a test can drive this without a terminal.
	program := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithOutput(out),
		tea.WithInput(in),
		tea.WithAltScreen(),
	)

	if _, err := program.Run(); err != nil {
		return fmt.Errorf("running the dashboard: %w", err)
	}
	return nil
}
