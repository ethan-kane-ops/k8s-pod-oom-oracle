package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/render"
)

// inspectTimeout bounds a query against the daemon.
const inspectTimeout = 10 * time.Second

type inspectConfig struct {
	daemonAddr string
	namespace  string
	container  string
	limit      int
	format     string
}

func newInspectCmd() *cobra.Command {
	cfg := inspectConfig{}

	cmd := &cobra.Command{
		Use:   "inspect [pod]",
		Short: "Render the OOM post-mortem for a pod",
		Long: `Render the post-mortem for OOM kills recorded by the daemon.

The pod argument accepts a bare name or the pod/<name> form kubectl uses. With
no argument, every recorded kill is listed newest first.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var pod string
			if len(args) == 1 {
				pod = strings.TrimPrefix(args[0], "pod/")
			}
			return runInspect(cmd.Context(), cmd.OutOrStdout(), cfg, pod)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.daemonAddr, "daemon", "http://127.0.0.1:9090", "base URL of the oom-oracle daemon")
	flags.StringVarP(&cfg.namespace, "namespace", "n", "", "filter by namespace")
	flags.StringVarP(&cfg.container, "container", "c", "", "filter by container name")
	flags.IntVar(&cfg.limit, "limit", 0, "maximum reports to render, newest first")
	flags.StringVarP(&cfg.format, "output", "o", formatText, "output format: text|json")

	return cmd
}

func runInspect(ctx context.Context, w io.Writer, cfg inspectConfig, pod string) error {
	if cfg.format != formatText && cfg.format != formatJSON {
		return fmt.Errorf("unknown output format %q: want %s or %s", cfg.format, formatText, formatJSON)
	}

	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()

	reports, err := fetchReports(ctx, cfg, pod)
	if err != nil {
		return err
	}

	if cfg.format == formatJSON {
		payload, err := render.JSONList(reports)
		if err != nil {
			return err
		}
		return writeAll(w, payload)
	}

	if len(reports) == 0 {
		return writeAll(w, []byte("no OOM kills recorded\n"))
	}

	for i := range reports {
		if i > 0 {
			if err := writeAll(w, []byte("\n"+strings.Repeat("─", 72)+"\n\n")); err != nil {
				return err
			}
		}
		if err := writeAll(w, []byte(render.Text(&reports[i], render.TextOptions{}))); err != nil {
			return err
		}
	}

	return nil
}

// fetchReports queries the daemon's list endpoint.
func fetchReports(ctx context.Context, cfg inspectConfig, pod string) ([]oom.Report, error) {
	endpoint, err := url.Parse(strings.TrimSuffix(cfg.daemonAddr, "/") + "/v1/events")
	if err != nil {
		return nil, fmt.Errorf("parsing daemon address %q: %w", cfg.daemonAddr, err)
	}

	query := endpoint.Query()
	setIfNotEmpty(query, "pod", pod)
	setIfNotEmpty(query, "namespace", cfg.namespace)
	setIfNotEmpty(query, "container", cfg.container)
	if cfg.limit > 0 {
		query.Set("limit", strconv.Itoa(cfg.limit))
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying daemon at %s: %w", cfg.daemonAddr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon returned %s", resp.Status)
	}

	var reports []oom.Report
	if err := json.NewDecoder(resp.Body).Decode(&reports); err != nil {
		return nil, fmt.Errorf("decoding daemon response: %w", err)
	}
	return reports, nil
}

func setIfNotEmpty(query url.Values, key, value string) {
	if value != "" {
		query.Set(key, value)
	}
}

func writeAll(w io.Writer, payload []byte) error {
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
