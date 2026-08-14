package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/api"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/cgroup"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/daemon"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/detector"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/k8s"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/procfs"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/sampler"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/store"
	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/version"
)

// daemonConfig holds the daemon command's flags.
type daemonConfig struct {
	cgroupRoot   string
	procRoot     string
	cgroupPrefix string
	detectorName string
	listenAddr   string
	sampleEvery  time.Duration
	pollEvery    time.Duration
	historySize  int
	retain       int
	includeAll   bool
	kubernetes   string
	nodeName     string
	kubeconfig   string
	logLevel     string
	logFormat    string
}

// Detector names accepted by --detector.
const (
	detectorAuto   = "auto"
	detectorPoller = "poller"
	detectorEBPF   = "ebpf"
)

// Modes accepted by --kubernetes.
const (
	kubernetesAuto = "auto"
	kubernetesOn   = "on"
	kubernetesOff  = "off"
)

func newDaemonCmd() *cobra.Command {
	cfg := daemonConfig{}

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Watch this node for OOM kills",
		Long: `Watch this node's cgroups for OOM kills and build a post-mortem for each one.

Intended to run as a privileged DaemonSet with the host's /sys/fs/cgroup and
/proc mounted read-only. Reports are served over HTTP for the inspect command
to read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDaemon(cmd.Context(), cmd, cfg)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&cfg.cgroupRoot, "cgroup-root", "/sys/fs/cgroup", "path to the cgroup hierarchy")
	flags.StringVar(&cfg.procRoot, "proc-root", "/proc", "path to the proc filesystem")
	flags.StringVar(&cfg.cgroupPrefix, "cgroup-prefix", "/", "limit watching to this cgroup subtree")
	flags.StringVar(&cfg.detectorName, "detector", detectorAuto, "detection method: auto|poller|ebpf")
	flags.StringVar(&cfg.listenAddr, "listen", ":9090", "address to serve the HTTP API on")
	flags.DurationVar(&cfg.sampleEvery, "sample-interval", sampler.DefaultInterval, "how often to sample memory")
	flags.DurationVar(&cfg.pollEvery, "poll-interval", detector.DefaultPollInterval, "how often to poll for kills")
	flags.IntVar(&cfg.historySize, "history", sampler.DefaultHistorySize, "memory samples retained per container")
	flags.IntVar(&cfg.retain, "retain", store.DefaultCapacity, "reports retained in memory")
	flags.BoolVar(&cfg.includeAll, "include-non-kubernetes", false, "also report kills outside the kubepods tree")
	flags.StringVar(&cfg.kubernetes, "kubernetes", kubernetesAuto,
		"resolve pod names via the API server: auto|on|off")
	flags.StringVar(&cfg.nodeName, "node-name", "",
		"node to watch pods on (defaults to the "+k8s.NodeNameEnv+" environment variable)")
	flags.StringVar(&cfg.kubeconfig, "kubeconfig", "",
		"kubeconfig to use instead of in-cluster credentials")
	flags.StringVar(&cfg.logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flags.StringVar(&cfg.logFormat, "log-format", "text", "log format: text|json")

	return cmd
}

func runDaemon(ctx context.Context, cmd *cobra.Command, cfg daemonConfig) error {
	log, err := newLogger(cmd.ErrOrStderr(), cfg.logLevel, cfg.logFormat)
	if err != nil {
		return err
	}

	// Stop on SIGINT or SIGTERM so a kubelet eviction shuts down cleanly.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cgroupFS, err := cgroup.New(os.DirFS(cfg.cgroupRoot))
	if err != nil {
		return fmt.Errorf("opening cgroup hierarchy at %s: %w", cfg.cgroupRoot, err)
	}
	log.Info("cgroup hierarchy detected", "root", cfg.cgroupRoot, "version", cgroupFS.Version())

	procFS := procfs.New(os.DirFS(cfg.procRoot))

	memorySampler, err := sampler.New(sampler.Options{
		FS:          cgroupFS,
		Prefix:      cfg.cgroupPrefix,
		Interval:    cfg.sampleEvery,
		HistorySize: cfg.historySize,
		Logger:      log,
	})
	if err != nil {
		return fmt.Errorf("building sampler: %w", err)
	}

	killDetector, err := buildDetector(cfg, cgroupFS, procFS, log)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := killDetector.Close(); closeErr != nil {
			log.Error("closing detector", "error", closeErr)
		}
	}()

	reports := store.NewMemory(cfg.retain)

	podCache, err := buildPodCache(cfg, log)
	if err != nil {
		return err
	}

	// A nil *PodCache in a non-nil interface would satisfy the nil check inside
	// the resolver and then panic on first use, so the interface is only given
	// a value when there is one.
	var pods correlate.PodLookup
	if podCache != nil {
		pods = podCache
	}

	oracle, err := daemon.New(daemon.Options{
		Detector:             killDetector,
		Sampler:              memorySampler,
		Store:                reports,
		Cgroup:               cgroupFS,
		Resolver:             correlate.NewResolver(pods),
		Proc:                 procFS,
		IncludeNonKubernetes: cfg.includeAll,
		Logger:               log,
	})
	if err != nil {
		return fmt.Errorf("building daemon: %w", err)
	}

	started := time.Now()
	server, err := api.New(api.Options{
		Addr:   cfg.listenAddr,
		Store:  reports,
		Ready:  oracle.Ready,
		Status: func() api.Status { return daemonStatus(oracle, cgroupFS, podCache, started) },
		Logger: log,
	})
	if err != nil {
		return fmt.Errorf("building api server: %w", err)
	}

	log.Info("serving api", "addr", cfg.listenAddr)

	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return oracle.Run(groupCtx) })
	group.Go(func() error { return server.ListenAndServe(groupCtx) })

	if podCache != nil {
		group.Go(func() error { return podCache.Run(groupCtx) })
		// Waiting for the cache in the background is deliberate. Detection is
		// this daemon's job and an unreachable API server must never delay it;
		// the only cost of an unsynced cache is that reports name pods by UID
		// until it catches up, which /v1/status reports.
		go awaitPodCache(groupCtx, podCache, log)
	}

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	log.Info("daemon stopped", "reports", oracle.Processed(), "skipped", oracle.Skipped())
	return nil
}

// buildDetector selects a detection method.
//
// auto prefers eBPF and falls back to polling, logging why. An explicit
// --detector=ebpf never falls back: a user who asked for traced victims should
// be told the kernel refused rather than handed inferred ones that look the
// same in every field except a boolean.
func buildDetector(
	cfg daemonConfig,
	cgroupFS *cgroup.FS,
	procFS *procfs.FS,
	log *slog.Logger,
) (detector.Detector, error) {
	switch cfg.detectorName {
	case detectorEBPF:
		return newEBPFDetector(cfg, cgroupFS, procFS, log)

	case detectorAuto:
		traced, err := newEBPFDetector(cfg, cgroupFS, procFS, log)
		if err == nil {
			log.Info("using the eBPF detector", "kprobe", "oom_kill_process")
			return traced, nil
		}
		log.Info("falling back to the polling detector",
			"reason", err,
			"effect", "victims are inferred from process snapshots, not traced")
		return newPollingDetector(cfg, cgroupFS, procFS, log)

	case detectorPoller:
		return newPollingDetector(cfg, cgroupFS, procFS, log)

	default:
		return nil, fmt.Errorf("unknown detector %q: want %s, %s, or %s",
			cfg.detectorName, detectorAuto, detectorPoller, detectorEBPF)
	}
}

// buildPodCache prepares cluster correlation.
//
// auto degrades to UID-only correlation with a logged reason, because this is a
// node diagnostic and must keep working when the control plane does not. An
// explicit --kubernetes=on never degrades: someone who asked for pod names
// should be told why they are absent rather than left reading UIDs and
// wondering.
func buildPodCache(cfg daemonConfig, log *slog.Logger) (*k8s.PodCache, error) {
	switch cfg.kubernetes {
	case kubernetesOff:
		return nil, nil

	case kubernetesOn, kubernetesAuto:
		podCache, err := newPodCache(cfg, log)
		if err == nil {
			log.Info("resolving pod names from the api server", "node", podCache.Node())
			return podCache, nil
		}
		if cfg.kubernetes == kubernetesOn {
			return nil, err
		}
		log.Info("continuing without cluster correlation",
			"reason", err,
			"effect", "reports identify pods by UID, not by namespace and name")
		return nil, nil

	default:
		return nil, fmt.Errorf("unknown --kubernetes value %q: want %s, %s, or %s",
			cfg.kubernetes, kubernetesAuto, kubernetesOn, kubernetesOff)
	}
}

func newPodCache(cfg daemonConfig, log *slog.Logger) (*k8s.PodCache, error) {
	node, err := k8s.NodeName(cfg.nodeName)
	if err != nil {
		return nil, err
	}

	restConfig, err := k8s.RestConfig(cfg.kubeconfig)
	if err != nil {
		return nil, err
	}

	client, err := k8s.NewClient(restConfig)
	if err != nil {
		return nil, err
	}

	return k8s.New(k8s.Options{Client: client, Node: node, Logger: log})
}

// awaitPodCache logs whether cluster correlation actually came up.
func awaitPodCache(ctx context.Context, podCache *k8s.PodCache, log *slog.Logger) {
	if err := podCache.WaitForSync(ctx, k8s.DefaultSyncTimeout); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.Warn("pod cache did not sync",
			"error", err,
			"effect", "reports identify pods by UID until it does")
		return
	}
	log.Info("pod cache synced", "node", podCache.Node(), "pods", podCache.Len())
}

// daemonStatus snapshots the daemon for /v1/status.
func daemonStatus(
	oracle *daemon.Daemon,
	cgroupFS *cgroup.FS,
	podCache *k8s.PodCache,
	started time.Time,
) api.Status {
	status := api.Status{
		Detector:       oracle.Detector(),
		CgroupVersion:  cgroupFS.Version().String(),
		Ready:          oracle.Ready(),
		Reports:        oracle.Processed(),
		Skipped:        oracle.Skipped(),
		TrackedCgroups: oracle.Tracked(),
		UptimeSeconds:  time.Since(started).Seconds(),
		Version:        version.Get().Version,
	}

	if podCache != nil {
		status.Node = podCache.Node()
		status.PodCacheSynced = podCache.Synced()
		status.PodsTracked = podCache.Len()
	}

	return status
}

func newEBPFDetector(
	cfg daemonConfig,
	cgroupFS *cgroup.FS,
	procFS *procfs.FS,
	log *slog.Logger,
) (detector.Detector, error) {
	return detector.NewEBPF(detector.EBPFOptions{
		CgroupRoot: cfg.cgroupRoot,
		Cgroup:     cgroupFS,
		Proc:       procFS,
		Logger:     log,
	})
}

func newPollingDetector(
	cfg daemonConfig,
	cgroupFS *cgroup.FS,
	procFS *procfs.FS,
	log *slog.Logger,
) (detector.Detector, error) {
	return detector.NewPoller(detector.PollerOptions{
		Cgroup:   cgroupFS,
		Proc:     procFS,
		Prefix:   cfg.cgroupPrefix,
		Interval: cfg.pollEvery,
		Logger:   log,
	})
}
