package k8s

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/ethan-kane-ops/k8s-pod-oom-oracle/internal/correlate"
)

const (
	testNode = "node-1"
	testUID  = "11111111-2222-3333-4444-555555555555"
)

// newTestCache builds a cache over a fake client without starting an informer,
// so handler logic can be driven directly and deterministically.
func newTestCache(t *testing.T, opts Options) *PodCache {
	t.Helper()

	if opts.Client == nil {
		opts.Client = fake.NewClientset()
	}
	if opts.Node == "" {
		opts.Node = testNode
	}

	podCache, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return podCache
}

// pod builds a pod carrying one running container.
func pod(uid, namespace, name, containerID string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Namespace: namespace,
			Name:      name,
		},
		Spec: corev1.PodSpec{NodeName: testNode},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "app",
				Image:       "alpine:3.20",
				ContainerID: containerID,
			}},
		},
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "no client", opts: Options{Node: testNode}},
		{name: "no node", opts: Options{Client: fake.NewClientset()}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.opts); err == nil {
				t.Fatal("New() error = nil, want an error")
			}
		})
	}
}

func TestLookupPodResolvesIdentity(t *testing.T) {
	podCache := newTestCache(t, Options{})
	podCache.upsert(pod(testUID, "team-a", "api-7d9", "containerd://abc123"))

	got, ok := podCache.LookupPod(testUID)
	if !ok {
		t.Fatal("LookupPod() ok = false, want true")
	}
	if got.Namespace != "team-a" || got.Name != "api-7d9" {
		t.Errorf("pod = %s/%s, want team-a/api-7d9", got.Namespace, got.Name)
	}
	if got.Node != testNode {
		t.Errorf("Node = %q, want %q", got.Node, testNode)
	}

	// The cgroup path carries the bare ID, so that is the only key a lookup can
	// ever be made with.
	container, found := got.Containers["abc123"]
	if !found {
		t.Fatalf("container abc123 not indexed; have %v", got.Containers)
	}
	if container.Name != "app" || container.Image != "alpine:3.20" {
		t.Errorf("container = %+v, want app/alpine:3.20", container)
	}
}

func TestLookupPodMissIsNotFound(t *testing.T) {
	podCache := newTestCache(t, Options{})

	if _, ok := podCache.LookupPod("no-such-uid"); ok {
		t.Error("LookupPod() ok = true for an unknown UID, want false")
	}
}

// TestPodInfoIndexesEveryContainerID covers the ID shapes a report can arrive
// carrying. The restarted case is the one that matters most: the kill names the
// dead container, while the pod status has already moved on to its replacement.
func TestPodInfoIndexesEveryContainerID(t *testing.T) {
	subject := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: testUID, Namespace: "team-a", Name: "api"},
		Spec:       corev1.PodSpec{NodeName: testNode},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "app",
				Image:       "alpine:3.20",
				ContainerID: "containerd://current",
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ContainerID: "containerd://previous",
						Reason:      "OOMKilled",
					},
				},
			}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:        "setup",
				Image:       "busybox:1.36",
				ContainerID: "cri-o://init-id",
			}},
			EphemeralContainerStatuses: []corev1.ContainerStatus{{
				Name:        "debug",
				Image:       "busybox:1.36",
				ContainerID: "docker://ephemeral-id",
			}},
		},
	}

	got := podInfo(subject)

	tests := []struct {
		containerID string
		want        string
	}{
		{containerID: "current", want: "app"},
		{containerID: "previous", want: "app"},
		{containerID: "init-id", want: "setup"},
		{containerID: "ephemeral-id", want: "debug"},
	}

	for _, test := range tests {
		t.Run(test.containerID, func(t *testing.T) {
			container, found := got.Containers[test.containerID]
			if !found {
				t.Fatalf("container %q not indexed; have %v", test.containerID, got.Containers)
			}
			if container.Name != test.want {
				t.Errorf("name = %q, want %q", container.Name, test.want)
			}
		})
	}
}

func TestPodInfoSkipsUnstartedContainers(t *testing.T) {
	subject := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: testUID},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{Name: "pending", ContainerID: ""}},
		},
	}

	if got := podInfo(subject); len(got.Containers) != 0 {
		t.Errorf("Containers = %v, want empty for a container that never started", got.Containers)
	}
}

// TestDeletedPodSurvivesItsGraceWindow is the behaviour the tool depends on: a
// container killed outright takes its pod with it, and the report is built just
// after.
func TestDeletedPodSurvivesItsGraceWindow(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	podCache := newTestCache(t, Options{Grace: time.Minute, now: clock})
	podCache.upsert(pod(testUID, "team-a", "api-7d9", "containerd://abc123"))
	podCache.remove(pod(testUID, "team-a", "api-7d9", "containerd://abc123"))

	got, ok := podCache.LookupPod(testUID)
	if !ok {
		t.Fatal("LookupPod() ok = false immediately after deletion, want true")
	}
	if got.Name != "api-7d9" {
		t.Errorf("Name = %q, want api-7d9", got.Name)
	}
	if podCache.Len() != 0 {
		t.Errorf("Len() = %d, want 0: a deleted pod is not live", podCache.Len())
	}

	// Past the window it is gone, so the graveyard cannot grow without bound.
	now = now.Add(2 * time.Minute)
	if _, ok := podCache.LookupPod(testUID); ok {
		t.Error("LookupPod() ok = true after the grace window, want false")
	}
}

func TestSweepEvictsExpiredTombstones(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	podCache := newTestCache(t, Options{Grace: time.Minute, now: clock})
	podCache.upsert(pod(testUID, "team-a", "old", "containerd://a"))
	podCache.remove(pod(testUID, "team-a", "old", "containerd://a"))

	// A later deletion is what triggers the sweep of the earlier one.
	now = now.Add(2 * time.Minute)
	const secondUID = "99999999-8888-7777-6666-555555555555"
	podCache.upsert(pod(secondUID, "team-a", "new", "containerd://b"))
	podCache.remove(pod(secondUID, "team-a", "new", "containerd://b"))

	podCache.mu.RLock()
	defer podCache.mu.RUnlock()

	if _, found := podCache.buried[testUID]; found {
		t.Error("expired tombstone was not swept")
	}
	if _, found := podCache.buried[secondUID]; !found {
		t.Error("the tombstone still inside its window was swept")
	}
}

// TestRemoveHandlesTombstone covers a watch that fell far enough behind that
// client-go delivers the last known state rather than the object.
func TestRemoveHandlesTombstone(t *testing.T) {
	podCache := newTestCache(t, Options{Grace: time.Minute})
	subject := pod(testUID, "team-a", "api-7d9", "containerd://abc123")
	podCache.upsert(subject)

	podCache.remove(cache.DeletedFinalStateUnknown{Key: "team-a/api-7d9", Obj: subject})

	if _, ok := podCache.LookupPod(testUID); !ok {
		t.Error("LookupPod() ok = false after a tombstone delete, want true within the grace window")
	}
	if podCache.Len() != 0 {
		t.Errorf("Len() = %d, want 0", podCache.Len())
	}
}

func TestRemoveIgnoresUnknownObjects(t *testing.T) {
	podCache := newTestCache(t, Options{})
	podCache.upsert(pod(testUID, "team-a", "api", "containerd://abc"))

	podCache.remove("not a pod")
	podCache.remove(cache.DeletedFinalStateUnknown{Key: "x", Obj: "not a pod"})

	if podCache.Len() != 1 {
		t.Errorf("Len() = %d, want 1: an undecodable delete must not drop live pods", podCache.Len())
	}
}

// TestUpsertClearsTombstone covers a resync re-adding a pod whose delete was
// observed but never actually happened.
func TestUpsertClearsTombstone(t *testing.T) {
	podCache := newTestCache(t, Options{Grace: time.Minute})
	subject := pod(testUID, "team-a", "api", "containerd://abc")

	podCache.upsert(subject)
	podCache.remove(subject)
	podCache.upsert(subject)

	podCache.mu.RLock()
	defer podCache.mu.RUnlock()

	if _, found := podCache.buried[testUID]; found {
		t.Error("tombstone survived the pod coming back")
	}
	if _, found := podCache.live[testUID]; !found {
		t.Error("pod is not live after being re-added")
	}
}

func TestTrimPodDropsBulk(t *testing.T) {
	subject := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "api",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
			Annotations: map[string]string{
				corev1.LastAppliedConfigAnnotation: "{...a large blob...}",
				"keep-me":                          "yes",
			},
		},
	}

	trimmed, err := trimPod(subject)
	if err != nil {
		t.Fatalf("trimPod() error = %v", err)
	}

	got, ok := trimmed.(*corev1.Pod)
	if !ok {
		t.Fatalf("trimPod() returned %T, want *corev1.Pod", trimmed)
	}
	if got.ManagedFields != nil {
		t.Error("managed fields survived the transform")
	}
	if _, found := got.Annotations[corev1.LastAppliedConfigAnnotation]; found {
		t.Error("last-applied annotation survived the transform")
	}
	if got.Annotations["keep-me"] != "yes" {
		t.Error("transform dropped an annotation it was not asked to")
	}
}

func TestTrimPodPassesThroughOtherTypes(t *testing.T) {
	tombstone := cache.DeletedFinalStateUnknown{Key: "x"}

	got, err := trimPod(tombstone)
	if err != nil {
		t.Fatalf("trimPod() error = %v", err)
	}
	if got != any(tombstone) {
		t.Error("trimPod() altered a non-pod object")
	}
}

// TestInformerScopesToNodeAndPopulates is the one test that runs a real
// informer. It proves both halves of the wiring: that the watch is restricted
// to this node, and that an event actually reaches the UID map.
func TestInformerScopesToNodeAndPopulates(t *testing.T) {
	client := fake.NewClientset(pod(testUID, "team-a", "api-7d9", "containerd://abc123"))
	podCache := newTestCache(t, Options{Client: client})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- podCache.Run(ctx) }()

	if err := podCache.WaitForSync(ctx, 10*time.Second); err != nil {
		t.Fatalf("WaitForSync() error = %v", err)
	}
	if !podCache.Synced() {
		t.Error("Synced() = false after a successful sync")
	}
	if podCache.Node() != testNode {
		t.Errorf("Node() = %q, want %q", podCache.Node(), testNode)
	}

	// Handlers are delivered asynchronously, so the map is polled rather than
	// read once.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := podCache.LookupPod(testUID); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pod never reached the cache")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The fake client does not enforce field selectors, so scoping is asserted
	// on what was requested rather than on what came back.
	var listed bool
	for _, action := range client.Actions() {
		list, ok := action.(k8stesting.ListAction)
		if !ok {
			continue
		}
		listed = true
		got := list.GetListRestrictions().Fields.String()
		if want := "spec.nodeName=" + testNode; got != want {
			t.Errorf("field selector = %q, want %q", got, want)
		}
	}
	if !listed {
		t.Fatal("informer issued no list")
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run() error = %v", err)
	}
}

func TestNodeName(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		env     string
		want    string
		wantErr bool
	}{
		{name: "flag wins", flag: "from-flag", env: "from-env", want: "from-flag"},
		{name: "env fallback", env: "from-env", want: "from-env"},
		{name: "neither is an error", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(NodeNameEnv, test.env)

			got, err := NodeName(test.flag)
			if test.wantErr {
				if err == nil {
					t.Fatal("NodeName() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("NodeName() error = %v", err)
			}
			if got != test.want {
				t.Errorf("NodeName() = %q, want %q", got, test.want)
			}
		})
	}
}

// TestPodCacheSatisfiesPodLookup keeps the two packages honest about the
// contract they are joined by.
func TestPodCacheSatisfiesPodLookup(t *testing.T) {
	var lookup correlate.PodLookup = newTestCache(t, Options{})

	if _, ok := lookup.LookupPod("absent"); ok {
		t.Error("LookupPod() ok = true on an empty cache")
	}
}
