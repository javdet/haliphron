// Package controller drives the real controller against FakeBackend and a real
// Kubernetes API server.
//
// The two halves are both necessary. FakeBackend is the only way to exercise
// the failures the protocol is built around — a stale epoch, an outage, a
// dropped long poll — because a backend that is up and agrees with us proves
// nothing. A real API server is the only way to exercise the CRD's own
// behaviour: CEL, defaulting and, above all, the silent pruning of fields an
// older schema does not know.
//
// What is deliberately absent is a kubelet and a Job controller. envtest has
// neither, so these tests create the pod and write its status themselves. That
// is not a limitation but the point: the phase table is a function of what the
// controller can see, and here the test decides exactly what that is.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"

	"github.com/automagicops/haliphron/controller/agentrun"
	"github.com/automagicops/haliphron/controller/callback"
	"github.com/automagicops/haliphron/controller/clusterapi"
	"github.com/automagicops/haliphron/controller/config"
	"github.com/automagicops/haliphron/controller/identity"
	"github.com/automagicops/haliphron/controller/launcher"
	"github.com/automagicops/haliphron/controller/lease"
	"github.com/automagicops/haliphron/controller/materialize"
	"github.com/automagicops/haliphron/controller/report"
)

const controllerVersion = "0.1.0"

var (
	restCfg *rest.Config
	scheme  = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, agentrunv1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			panic(err)
		}
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}
	restCfg = cfg
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

// clock is a controllable now. The deadlines in this contract — the startup
// budget, the bounded finalizer wait, the TTL — are all measured in minutes or
// hours, and a test that waited for them would not be run.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock { return &clock{at: time.Now()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// memoryStore is the identity Store without a Secret; registration is exercised
// here, not the persistence, which has a test of its own.
type memoryStore struct {
	mu sync.Mutex
	p  identity.Persisted
	ok bool
}

func (s *memoryStore) Load(context.Context) (identity.Persisted, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.p, s.ok, nil
}

func (s *memoryStore) Save(_ context.Context, p identity.Persisted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p, s.ok = p, true
	return nil
}

// harness is one controller, one FakeBackend and one namespace.
type harness struct {
	t   *testing.T
	ctx context.Context

	Backend    *fakebackend.Backend
	BackendURL string
	K8s        client.Client
	API        *clusterapi.Client
	Lease      *lease.Loop
	Reporter   *report.Reporter
	Reconcile  *agentrun.Reconciler
	Material   *materialize.Materializer
	Callback   *callback.Server
	Clock      *clock
	Timings    *config.Timings

	Namespace string
	ClusterID runv1.ULID
	Store     *memoryStore
}

type harnessOptions struct {
	timings      *clusterv1.Timings
	preflightJob bool
	k8s          client.Client
	store        *memoryStore
	namespace    string
	backend      *fakebackend.Backend
	backendURL   string
	faults       *faults
}

type harnessOption func(*harnessOptions)

// withTimings shortens the protocol's intervals so that a test can wait on them.
func withTimings(t clusterv1.Timings) harnessOption {
	return func(o *harnessOptions) { o.timings = &t }
}

// withClient substitutes a client, for the one test that needs writes to behave
// like an older CRD.
func withClient(c client.Client) harnessOption {
	return func(o *harnessOptions) { o.k8s = c }
}

// withStore reuses an identity, which is how a controller restart is staged.
func withStore(s *memoryStore) harnessOption {
	return func(o *harnessOptions) { o.store = s }
}

func withNamespace(ns string) harnessOption {
	return func(o *harnessOptions) { o.namespace = ns }
}

// withBackend reuses a running control plane, which is what makes a restart a
// restart rather than a migration to a backend that has never heard of this
// cluster.
// withFaults puts a switchboard in front of the control plane, so that one
// endpoint can fail while the rest keep working. The interesting failures in
// this protocol are partial: the lease arrives and the acknowledgement does not.
func withFaults(f *faults) harnessOption {
	return func(o *harnessOptions) { o.faults = f }
}

func withBackend(b *fakebackend.Backend, url string) harnessOption {
	return func(o *harnessOptions) { o.backend, o.backendURL = b, url }
}

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	options := harnessOptions{preflightJob: true}
	for _, opt := range opts {
		opt(&options)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// A short long poll by default: these tests exercise the protocol, not the
	// patience of the person running them.
	timing := clusterv1.DefaultTimings()
	timing.MaxWaitSeconds = 1
	timing.HeartbeatIntervalSeconds = 1
	if options.timings != nil {
		timing = *options.timings
	}

	backend := options.backend
	backendURL := options.backendURL
	if backend == nil {
		backend = fakebackend.New(fakebackend.WithTimings(timing))
		var handler http.Handler = backend.Handler()
		if options.faults != nil {
			handler = options.faults.wrap(handler)
		}
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		backendURL = server.URL
	}

	k8s := options.k8s
	if k8s == nil {
		var err error
		k8s, err = client.New(restCfg, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatalf("kubernetes client: %v", err)
		}
	}

	namespace := options.namespace
	if namespace == "" {
		namespace = uniqueNamespace(t)
	}
	ensureNamespace(t, ctx, k8s, namespace)

	log := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	clk := newClock()

	signer := &identity.Deferred{}
	api, err := clusterapi.New(clusterapi.Options{
		BaseURL:           backendURL,
		ControllerVersion: controllerVersion,
		Signer:            signer,
		Logger:            log,
		// Deliberately the real clock, not the test's. Tokens live five
		// minutes and the backend checks them against its own clock; a test
		// that skips an hour to reach a deadline must not thereby mint tokens
		// from the future.
	})
	if err != nil {
		t.Fatalf("cluster api client: %v", err)
	}

	store := options.store
	if store == nil {
		store = &memoryStore{}
	}
	id, registration, err := identity.Bootstrap(ctx, store, api, backend.BootstrapToken(),
		clusterv1.RegisterRequest{
			Name:              "test-cluster",
			ControllerVersion: controllerVersion,
			AgentNamespace:    namespace,
			CapacitySlots:     4,
			CRDVersions:       []string{agentrunv1alpha1.GroupVersion.Version},
		}, log)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	signer.Set(id)

	timings := config.NewTimings()
	if registration != nil {
		timings.Set(&registration.Timings)
	}

	builder := launcher.Builder{
		Namespace:            namespace,
		ClusterID:            id.ClusterID(),
		GraceSeconds:         30,
		DeadlineSlackSeconds: 600,
	}

	reporter, err := report.New(report.Config{
		API: api, K8s: k8s, Timings: timings,
		Namespace: namespace, ClusterID: id.ClusterID(),
		Capacity: 4, Version: controllerVersion, Clock: clk.Now, Log: log,
	})
	if err != nil {
		t.Fatalf("reporter: %v", err)
	}

	reconciler, err := agentrun.New(agentrun.Config{
		Client: k8s, Scheme: scheme, ClusterID: id.ClusterID(),
		Builder: builder, Notifier: reporter, Bundles: api,
		Clock: clk.Now, Log: log,
	})
	if err != nil {
		t.Fatalf("reconciler: %v", err)
	}

	materializer := &materialize.Materializer{
		Client: k8s, Namespace: namespace, ClusterID: id.ClusterID(),
		CallbackURL:  "http://haliphron-controller." + namespace + ".svc:8083/completion",
		Builder:      builder,
		PreflightJob: options.preflightJob,
		Clock:        clk.Now,
		Log:          log,
	}

	leaseLoop, err := lease.New(lease.Loop{
		API: api, K8s: k8s, Materializer: materializer, Timings: timings,
		Namespace: namespace, ClusterID: id.ClusterID(), Capacity: 4,
		Clock: clk.Now, Log: log,
	})
	if err != nil {
		t.Fatalf("lease loop: %v", err)
	}

	callbackServer, err := callback.New(callback.Config{
		K8s: k8s, Namespace: namespace, Sink: reporter,
		CallbackURL: materializer.CallbackURL, Clock: clk.Now, Log: log,
	})
	if err != nil {
		t.Fatalf("callback server: %v", err)
	}

	return &harness{
		t: t, ctx: ctx,
		Backend: backend, BackendURL: backendURL, K8s: k8s, API: api,
		Lease: leaseLoop, Reporter: reporter, Reconcile: reconciler, Callback: callbackServer,
		Material: materializer,
		Clock:    clk, Timings: timings,
		Namespace: namespace, ClusterID: id.ClusterID(), Store: store,
	}
}

// restart builds a second controller over the same cluster state and the same
// identity, which is what a rescheduled pod is.
func (h *harness) restart() *harness {
	h.t.Helper()
	return newHarness(h.t,
		withClient(h.K8s), withStore(h.Store), withNamespace(h.Namespace),
		withBackend(h.Backend, h.BackendURL))
}

// faults fails chosen endpoints without taking the control plane down.
type faults struct {
	mu      sync.Mutex
	failing map[string]bool
}

func newFaults() *faults { return &faults{failing: map[string]bool{}} }

// Fail makes every request whose path ends with suffix answer 503 with an
// action of backoff, which is what an overloaded or restarting control plane
// looks like from inside a cluster.
func (f *faults) Fail(suffix string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[suffix] = on
}

func (f *faults) shouldFail(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for suffix, on := range f.failing {
		if on && strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (f *faults) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.shouldFail(r.URL.Path) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"type":"https://haliphron.io/problems/unavailable",` +
				`"title":"injected","status":503,"code":"Unavailable","action":"backoff"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

var namespaceCounter struct {
	sync.Mutex
	n int
}

func uniqueNamespace(t *testing.T) string {
	namespaceCounter.Lock()
	defer namespaceCounter.Unlock()
	namespaceCounter.n++
	name := fmt.Sprintf("ns-%d-%d", time.Now().UnixNano()%1e6, namespaceCounter.n)
	return strings.ToLower(name)
}

func ensureNamespace(t *testing.T, ctx context.Context, c client.Client, name string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

// ---------------------------------------------------------------------------
// driving the controller
// ---------------------------------------------------------------------------

// poll performs one lease poll and returns how many leases arrived.
func (h *harness) poll() int {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	n, err := h.Lease.PollOnce(ctx)
	if err != nil {
		h.t.Fatalf("poll: %v", err)
	}
	return n
}

// pollErr is poll for the tests that are about the failure.
func (h *harness) pollErr() error {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	_, err := h.Lease.PollOnce(ctx)
	return err
}

// reconcileOnce runs one pass of the reconciler over a run.
func (h *harness) reconcileOnce(runID runv1.ULID) reconcile.Result {
	h.t.Helper()
	res, err := h.Reconcile.Reconcile(h.ctx, reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: h.Namespace, Name: agentrunv1alpha1.ObjectName(runID),
	}})
	if err != nil {
		h.t.Fatalf("reconcile %s: %v", runID, err)
	}
	return res
}

// reconcile runs the reconciler until it stops asking to be requeued
// immediately. Each pass is one decision — observe, then act on what was
// observed — so several are needed before the cluster catches up with reality.
func (h *harness) reconcile(runID runv1.ULID) {
	h.t.Helper()
	for i := 0; i < 8; i++ {
		res := h.reconcileOnce(runID)
		if !res.Requeue && res.RequeueAfter == 0 {
			return
		}
		if res.RequeueAfter > 0 && !res.Requeue {
			return
		}
	}
}

func (h *harness) flush() {
	h.t.Helper()
	if err := h.Reporter.Flush(h.ctx); err != nil {
		h.t.Fatalf("flush: %v", err)
	}
}

func (h *harness) beat() {
	h.t.Helper()
	if err := h.Reporter.Beat(h.ctx); err != nil {
		h.t.Fatalf("heartbeat: %v", err)
	}
}

// ---------------------------------------------------------------------------
// reading the cluster
// ---------------------------------------------------------------------------

func (h *harness) run(runID runv1.ULID) *agentrunv1alpha1.AgentRun {
	h.t.Helper()
	cr, err := h.runErr(runID)
	if err != nil {
		h.t.Fatalf("get AgentRun %s: %v", runID, err)
	}
	return cr
}

func (h *harness) runErr(runID runv1.ULID) (*agentrunv1alpha1.AgentRun, error) {
	var cr agentrunv1alpha1.AgentRun
	err := h.K8s.Get(h.ctx, types.NamespacedName{
		Namespace: h.Namespace, Name: agentrunv1alpha1.ObjectName(runID),
	}, &cr)
	if err != nil {
		return nil, err
	}
	return &cr, nil
}

func (h *harness) runGone(runID runv1.ULID) bool {
	h.t.Helper()
	cr, err := h.runErr(runID)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		h.t.Fatalf("get AgentRun %s: %v", runID, err)
	}
	// envtest runs no garbage collector, so an object held by a finalizer stays
	// visible. Being on the way out is as gone as it gets here.
	return cr.DeletionTimestamp != nil
}

// forceDelete removes a run the way losing a namespace would: without the
// controller being told, and without its finalizer being honoured. envtest has
// no garbage collector, so the finalizer has to be stripped by hand.
func (h *harness) forceDelete(runID runv1.ULID) {
	h.t.Helper()
	cr := h.run(runID)
	cr.Finalizers = nil
	if err := h.K8s.Update(h.ctx, cr); err != nil {
		h.t.Fatalf("strip finalizers: %v", err)
	}
	if err := h.K8s.Delete(h.ctx, cr); err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("delete AgentRun: %v", err)
	}
}

func (h *harness) job(runID runv1.ULID, attempt int32) *batchv1.Job {
	h.t.Helper()
	var job batchv1.Job
	err := h.K8s.Get(h.ctx, types.NamespacedName{
		Namespace: h.Namespace, Name: agentrunv1alpha1.JobName(runID, attempt),
	}, &job)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("get job: %v", err)
	}
	return &job
}

func (h *harness) secret(runID runv1.ULID) *corev1.Secret {
	h.t.Helper()
	var secret corev1.Secret
	err := h.K8s.Get(h.ctx, types.NamespacedName{
		Namespace: h.Namespace, Name: agentrunv1alpha1.SecretName(runID),
	}, &secret)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("get secret: %v", err)
	}
	return &secret
}

func (h *harness) configMap(runID runv1.ULID) *corev1.ConfigMap {
	h.t.Helper()
	var cm corev1.ConfigMap
	err := h.K8s.Get(h.ctx, types.NamespacedName{
		Namespace: h.Namespace, Name: agentrunv1alpha1.ConfigMapName(runID),
	}, &cm)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatalf("get configmap: %v", err)
	}
	return &cm
}

// ---------------------------------------------------------------------------
// standing in for the kubelet
// ---------------------------------------------------------------------------

// startPod creates the pod the Job controller would have created, with the
// labels its template carries.
func (h *harness) startPod(runID runv1.ULID, attempt int32) *corev1.Pod {
	h.t.Helper()
	job := h.job(runID, attempt)
	if job == nil {
		h.t.Fatalf("no job for %s attempt %d to make a pod for", runID, attempt)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-abcde",
			Namespace: h.Namespace,
			Labels:    job.Spec.Template.Labels,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  launcher.ContainerName,
				Image: job.Spec.Template.Spec.Containers[0].Image,
			}},
			NodeName: "node-1",
		},
	}
	if err := h.K8s.Create(h.ctx, pod); err != nil {
		h.t.Fatalf("create pod: %v", err)
	}
	return pod
}

func (h *harness) podRunning(pod *corev1.Pod) {
	h.t.Helper()
	pod.Status = corev1.PodStatus{
		Phase:     corev1.PodRunning,
		StartTime: ptr(metav1.NewTime(h.Clock.Now())),
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  launcher.ContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(h.Clock.Now())}},
		}},
	}
	h.updatePodStatus(pod)
}

func (h *harness) podWaiting(pod *corev1.Pod, reason string) {
	h.t.Helper()
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  launcher.ContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}},
		}},
	}
	h.updatePodStatus(pod)
}

func (h *harness) podExited(pod *corev1.Pod, code int32, reason string) {
	h.t.Helper()
	phase := corev1.PodSucceeded
	if code != 0 {
		phase = corev1.PodFailed
	}
	pod.Status = corev1.PodStatus{
		Phase:     phase,
		StartTime: ptr(metav1.NewTime(h.Clock.Now())),
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: launcher.ContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   code,
				Reason:     reason,
				StartedAt:  metav1.NewTime(h.Clock.Now()),
				FinishedAt: metav1.NewTime(h.Clock.Now()),
			}},
		}},
	}
	h.updatePodStatus(pod)
}

func (h *harness) updatePodStatus(pod *corev1.Pod) {
	h.t.Helper()
	if err := h.K8s.Status().Update(h.ctx, pod); err != nil {
		h.t.Fatalf("update pod status: %v", err)
	}
}

func (h *harness) deletePod(pod *corev1.Pod) {
	h.t.Helper()
	if err := h.K8s.Delete(h.ctx, pod, client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatalf("delete pod: %v", err)
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// sampleSpec is a run with a repository, because the interesting half of the
// runtime contract only exists when there is one.
func sampleSpec() runv1.RenderedRunSpec {
	createPR := true
	return runv1.RenderedRunSpec{
		Agent:  runv1.AgentClaudeCode,
		Prompt: runv1.ObjectRef{Bucket: "haliphron", Key: "runs/x/prompt.txt", SHA256: strings.Repeat("a", 64)},
		Model:  "anthropic/claude-opus-5",
		Image:  "ghcr.io/automagicops/agent:1.0.0",
		Repo: runv1.RepoSpec{
			URL:          "https://github.com/acme/widgets",
			Provider:     runv1.GitProviderGitHub,
			BaseBranch:   "main",
			TargetBranch: "haliphron/run-x",
			CreatePR:     &createPR,
		},
		Runtime: runv1.RuntimeSpec{
			TimeoutSeconds: 3600,
			Resources: runv1.Resources{
				CPU: "2", Memory: "4Gi", EphemeralStorage: "20Gi",
			},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func attemptLabel(n int32) string { return strconv.Itoa(int(n)) }
