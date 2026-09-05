// Package tui is the live terminal dashboard.
//
// It is a reader of the daemon's HTTP API and holds no detection logic of its
// own. The detail pane renders through internal/render, the same function
// `oom-oracle inspect` prints, so the dashboard and the CLI cannot disagree
// about what a report says.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/api"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/oom"
)

// fetchTimeout bounds one round of requests.
//
// It is shorter than the refresh interval on purpose: a daemon that has stopped
// answering should surface as an error on the next tick rather than stack
// requests until something gives way.
const fetchTimeout = 5 * time.Second

// Snapshot is everything one refresh collects.
type Snapshot struct {
	Status  api.Status
	Reports []oom.Report
}

// Client fetches daemon state.
//
// It is an interface so the model can be driven from a fixture. Every test in
// this package would otherwise need an HTTP server to assert on a layout.
type Client interface {
	Fetch(ctx context.Context) (Snapshot, error)
}

// HTTPClient reads a running daemon.
type HTTPClient struct {
	// Addr is the daemon base URL, such as http://127.0.0.1:9090.
	Addr string
	// HTTP is the client to use. Nil means http.DefaultClient.
	HTTP *http.Client
}

var _ Client = (*HTTPClient)(nil)

// Fetch reads status and reports.
//
// Status is fetched first because it is what the dashboard can still show when
// there are no reports at all, which is the normal state of a healthy node. A
// failure to read it is therefore the failure worth reporting.
func (c *HTTPClient) Fetch(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	var snapshot Snapshot
	if err := c.get(ctx, "/v1/status", &snapshot.Status); err != nil {
		return Snapshot{}, err
	}
	if err := c.get(ctx, "/v1/events", &snapshot.Reports); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (c *HTTPClient) get(ctx context.Context, path string, into any) error {
	endpoint, err := url.Parse(strings.TrimSuffix(c.Addr, "/") + path)
	if err != nil {
		return fmt.Errorf("parsing daemon address %q: %w", c.Addr, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}

	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("querying daemon at %s: %w", c.Addr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon returned %s for %s", resp.Status, path)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}
