package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
)

// Defaults for a PodCache.
const (
	// DefaultResync is how often the informer re-lists. Changes arrive by watch,
	// so this only repairs a silently dropped event and can be lazy.
	DefaultResync = 10 * time.Minute
	// DefaultGrace is how long a deleted pod stays resolvable. See burial.
	DefaultGrace = 5 * time.Minute
	// DefaultSyncTimeout bounds the initial list.
	DefaultSyncTimeout = 10 * time.Second
)

// Options configures a PodCache.
type Options struct {
	// Client talks to the API server. Required.
	Client kubernetes.Interface
	// Node restricts the informer to pods scheduled here. Required.
	Node string
	// Resync is the informer resync period. Zero means DefaultResync.
	Resync time.Duration
	// Grace is how long deleted pods remain resolvable. Zero means DefaultGrace.
	Grace time.Duration
	// Logger receives operational messages.
	Logger *slog.Logger

	// now is the clock, overridden by tests to exercise expiry without sleeping.
	now func() time.Time
}

// PodCache resolves pod UIDs to cluster identities from a node-scoped informer.
//
// It satisfies correlate.PodLookup. Lookups are keyed by UID because that is
// what a cgroup path carries; the informer's own store is keyed by namespace
// and name and so cannot answer the only question asked of it.
type PodCache struct {
	factory  informers.SharedInformerFactory
	informer cache.SharedIndexInformer
	node     string
	grace    time.Duration
	log      *slog.Logger
	now      func() time.Time

	mu     sync.RWMutex
	live   map[string]correlate.PodInfo
	buried map[string]burial

	synced atomic.Bool
}

// burial is a deleted pod held back from collection for a while.
//
// A container killed outright takes its pod with it, and under a controller the
// pod is replaced within seconds. If the delete event beats report construction
// the report loses its identity at precisely the moment the tool exists to
// explain, so deletion starts a grace period rather than dropping the entry.
type burial struct {
	pod     correlate.PodInfo
	expires time.Time
}

var _ correlate.PodLookup = (*PodCache)(nil)

// New builds a pod cache. It registers the informer but starts nothing; call
// Run for that.
func New(opts Options) (*PodCache, error) {
	switch {
	case opts.Client == nil:
		return nil, errors.New("pod cache requires a kubernetes client")
	case opts.Node == "":
		return nil, errors.New("pod cache requires a node name")
	}

	if opts.Resync == 0 {
		opts.Resync = DefaultResync
	}
	if opts.Grace == 0 {
		opts.Grace = DefaultGrace
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.now == nil {
		opts.now = time.Now
	}

	factory := informers.NewSharedInformerFactoryWithOptions(
		opts.Client,
		opts.Resync,
		// Scoping the watch to this node is what keeps a DaemonSet's load on the
		// API server proportional to the node rather than to the cluster.
		informers.WithTweakListOptions(func(list *metav1.ListOptions) {
			list.FieldSelector = fields.OneTermEqualSelector("spec.nodeName", opts.Node).String()
		}),
	)

	podCache := &PodCache{
		factory: factory,
		node:    opts.Node,
		grace:   opts.Grace,
		log:     opts.Logger,
		now:     opts.now,
		live:    make(map[string]correlate.PodInfo),
		buried:  make(map[string]burial),
	}

	informer := factory.Core().V1().Pods().Informer()
	if err := informer.SetTransform(trimPod); err != nil {
		return nil, fmt.Errorf("setting pod transform: %w", err)
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { podCache.upsert(obj) },
		UpdateFunc: func(_, obj any) { podCache.upsert(obj) },
		DeleteFunc: podCache.remove,
	}); err != nil {
		return nil, fmt.Errorf("registering pod handlers: %w", err)
	}
	podCache.informer = informer

	return podCache, nil
}

// Run starts the informer and blocks until the context is cancelled.
func (p *PodCache) Run(ctx context.Context) error {
	p.factory.Start(ctx.Done())
	<-ctx.Done()
	// Shutdown waits for the informer goroutines to finish, which is what stops
	// a handler from writing to the maps after Run has returned.
	p.factory.Shutdown()
	return nil
}

// WaitForSync blocks until the initial list completes, the timeout expires, or
// the context is cancelled.
//
// Callers are expected to treat failure as degradation rather than as fatal:
// the informer keeps retrying, so an API server that comes back later fills the
// cache without a restart.
func (p *PodCache) WaitForSync(ctx context.Context, timeout time.Duration) error {
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if !cache.WaitForCacheSync(syncCtx.Done(), p.informer.HasSynced) {
		return fmt.Errorf("pod cache did not sync within %s", timeout)
	}

	p.synced.Store(true)
	return nil
}

// LookupPod satisfies correlate.PodLookup.
func (p *PodCache) LookupPod(podUID string) (correlate.PodInfo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if pod, ok := p.live[podUID]; ok {
		return pod, true
	}
	if grave, ok := p.buried[podUID]; ok && !p.now().After(grave.expires) {
		return grave.pod, true
	}

	return correlate.PodInfo{}, false
}

// Synced reports whether the initial list has completed.
func (p *PodCache) Synced() bool { return p.synced.Load() }

// Node names the node being watched.
func (p *PodCache) Node() string { return p.node }

// Len reports how many live pods are held.
func (p *PodCache) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.live)
}

// upsert records a pod under its UID.
func (p *PodCache) upsert(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}

	info := podInfo(pod)
	uid := string(pod.UID)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.live[uid] = info
	// A resync can re-add a pod whose delete was observed but never really
	// happened, so any tombstone for it is now stale.
	delete(p.buried, uid)
}

// remove moves a pod to the graveyard rather than dropping it.
func (p *PodCache) remove(obj any) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// A watch that fell far enough behind delivers the last known state
		// wrapped in a tombstone instead of the object.
		tombstone, isTombstone := obj.(cache.DeletedFinalStateUnknown)
		if !isTombstone {
			return
		}
		if pod, ok = tombstone.Obj.(*corev1.Pod); !ok {
			return
		}
	}

	uid := string(pod.UID)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Prefer what was already recorded. A delete can carry a stripped-down
	// object, whereas the stored copy was built from a full one.
	info, known := p.live[uid]
	if !known {
		info = podInfo(pod)
	}

	delete(p.live, uid)
	p.buried[uid] = burial{pod: info, expires: p.now().Add(p.grace)}

	p.sweep()
}

// sweep drops expired tombstones. The caller must hold the write lock.
//
// Sweeping on delete bounds the graveyard by the node's pod churn rather than
// by its uptime, and costs a walk of a map that only holds pods deleted within
// the grace window.
func (p *PodCache) sweep() {
	now := p.now()
	for uid, grave := range p.buried {
		if now.After(grave.expires) {
			delete(p.buried, uid)
		}
	}
}

// podInfo converts a pod into the subset the resolver needs.
func podInfo(pod *corev1.Pod) correlate.PodInfo {
	info := correlate.PodInfo{
		Namespace:  pod.Namespace,
		Name:       pod.Name,
		Node:       pod.Spec.NodeName,
		Containers: make(map[string]correlate.ContainerInfo),
	}

	add := func(name, image, containerID string) {
		if containerID == "" {
			return
		}
		// The API server reports "containerd://<hex>" while a cgroup path
		// carries the bare hex.
		info.Containers[correlate.StripRuntimePrefix(containerID)] = correlate.ContainerInfo{
			Name:  name,
			Image: image,
		}
	}

	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.ContainerStatuses)+
		len(pod.Status.InitContainerStatuses)+len(pod.Status.EphemeralContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.EphemeralContainerStatuses...)

	for _, status := range statuses {
		add(status.Name, status.Image, status.ContainerID)
		// A container that was OOM killed and restarted carries a new ID, while
		// the cgroup path recorded against the kill still names the dead one.
		// Indexing the previous ID too is what lets such a report name the
		// container that died rather than only the pod it lived in.
		if last := status.LastTerminationState.Terminated; last != nil {
			add(status.Name, status.Image, last.ContainerID)
		}
	}

	return info
}

// trimPod drops the parts of a pod nothing here reads.
//
// The informer holds every pod on the node for the daemon's lifetime, and the
// daemon ships with a 128Mi limit. Managed fields and the last-applied
// annotation are routinely larger than the rest of the object combined, and an
// OOM diagnostic that OOMs on its own cache would be a poor advertisement.
func trimPod(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// Tombstones and unknown types pass through untouched.
		return obj, nil
	}

	pod.ManagedFields = nil
	delete(pod.Annotations, corev1.LastAppliedConfigAnnotation)

	return pod, nil
}
