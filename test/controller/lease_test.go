package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	fakebackend "github.com/automagicops/haliphron/fake/backend"
)

// The lease path: from a run queued in the control plane to objects in a
// cluster, and back again as an acknowledgement.

// TestALeaseBecomesDurableAndIsAcknowledged is the happy path, and the
// acknowledgement is the part that matters: it is the moment the backend may
// stop regarding the work as reassignable, and it must not be sent before the
// objects exist.
func TestALeaseBecomesDurableAndIsAcknowledged(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())

	if got := h.poll(); got != 1 {
		t.Fatalf("want one lease, got %d", got)
	}

	cr := h.run(id)
	if cr.Spec.RunID != id || cr.Spec.LeaseEpoch != 1 {
		t.Fatalf("the AgentRun does not describe the lease: %s epoch %d", cr.Spec.RunID, cr.Spec.LeaseEpoch)
	}
	if h.secret(id) == nil {
		t.Fatal("the per-run Secret was not created; the pod would start with no token")
	}

	state, _ := h.Backend.RunState(id)
	if !state.Acked || state.Status != clusterv1.StatusDispatched {
		t.Fatalf("want an acknowledged, dispatched run, got acked=%t status=%s", state.Acked, state.Status)
	}
}

// TestTheJobWaitsForTheAcknowledgement is the invariant the ack deadline rests
// on. The backend reassigns an unacknowledged lease on the grounds that the
// work cannot have started; a controller that created the pod first would make
// that a lie, and two clusters would run the same agent against the same branch.
func TestTheJobWaitsForTheAcknowledgement(t *testing.T) {
	faults := newFaults()
	h := newHarness(t, withFaults(faults))
	id := h.Backend.Enqueue(sampleSpec())

	faults.Fail("/ack", true)
	h.poll()

	cr := h.run(id)
	if _, ok := cr.Annotations[agentrunv1alpha1.GroupName+"/acknowledged-at"]; ok {
		t.Fatal("the run was marked acknowledged although the call failed")
	}
	h.reconcile(id)
	if job := h.job(id, 1); job != nil {
		t.Fatal("a Job was created for a lease the control plane never confirmed")
	}

	faults.Fail("/ack", false)
	h.Lease.Resume(h.ctx)
	h.reconcile(id)
	if job := h.job(id, 1); job == nil {
		t.Fatal("the Job was not created after the acknowledgement went through")
	}
	if state, _ := h.Backend.RunState(id); !state.Acked {
		t.Fatal("the repeated acknowledgement did not reach the control plane")
	}
}

// TestARestartRepeatsTheAcknowledgement is the same property across a process
// boundary: the objects are in the cluster, the call never went out, and the
// controller that comes up next has to finish the job the previous one started.
func TestARestartRepeatsTheAcknowledgement(t *testing.T) {
	faults := newFaults()
	h := newHarness(t, withFaults(faults))
	id := h.Backend.Enqueue(sampleSpec())

	faults.Fail("/ack", true)
	h.poll()
	faults.Fail("/ack", false)

	next := h.restart()
	next.Lease.Resume(next.ctx)

	state, _ := h.Backend.RunState(id)
	if !state.Acked {
		t.Fatal("a restarted controller did not repeat the acknowledgement")
	}
	if state.Epoch != 1 {
		t.Fatalf("the repeat used a different epoch: %d", state.Epoch)
	}
}

// TestSecretMaterialNeverReachesTheAgentRun is the boundary rule, checked where
// it can actually fail. `get agentruns` is a permission an operator will hand
// out; it must not be a way to read an hour-long git token.
func TestSecretMaterialNeverReachesTheAgentRun(t *testing.T) {
	h := newHarness(t)
	const token = "ghp_averysecrettokenvalue"
	id := h.Backend.Enqueue(sampleSpec(),
		fakebackend.WithSecrets(map[string]string{
			runv1.SecretKeyGitToken:  token,
			runv1.SecretKeyLLMAPIKey: "sk-ant-secret",
		}),
		fakebackend.WithRoleConfig(map[string]string{
			"CLAUDE.md": "you are a careful engineer",
		}))
	h.poll()

	cr := h.run(id)
	encoded, err := json.Marshal(cr.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	for _, secret := range []string{token, "sk-ant-secret", "sig=fake"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret material %q is in the AgentRun's spec", secret)
		}
	}

	secret := h.secret(id)
	if string(secret.Data[runv1.SecretKeyGitToken]) != token {
		t.Fatal("the git token did not reach the Secret")
	}
	// The prompt is a Secret key rather than a spec field. It is not a
	// credential, and it is here for the reason the credentials are: in the
	// spec it would land in `kubectl get agentrun -o yaml` and in every GitOps
	// diff.
	if len(secret.Data[runv1.SecretKeyPrompt]) == 0 {
		t.Fatal("the prompt did not reach the Secret; the pod would have no task")
	}
	// And no presigned bundle, because this run is in the default relay mode:
	// there is nothing to sign, and an empty bundle would give the entrypoint a
	// mode to misread.
	if len(secret.Data[runv1.SecretKeyPresigned]) != 0 {
		t.Fatal("a relay run was given a presigned bundle")
	}
	if len(secret.Data[runv1.SecretKeyCallbackToken]) == 0 {
		t.Fatal("no callback token was minted; any pod in the namespace could forge a completion")
	}

	cm := h.configMap(id)
	if cm == nil || cm.Data["CLAUDE.md"] == "" {
		t.Fatal("the role config did not become a ConfigMap")
	}
	if cr.Spec.Materials.ConfigMapName != cm.Name {
		t.Fatalf("the CR names %q as its ConfigMap, which is not %q", cr.Spec.Materials.ConfigMapName, cm.Name)
	}
	if strings.Contains(string(encoded), "careful engineer") {
		t.Fatal("the role config's contents are in the spec; only its ConfigMap's name belongs there")
	}
}

// TestNoWorkIsNotAFailure pins the 204. It is the ordinary answer on an idle
// queue, and a controller that treated it as an error would back off and turn
// an idle cluster into a slow one.
func TestNoWorkIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	start := time.Now()
	if got := h.poll(); got != 0 {
		t.Fatalf("want no leases on an empty queue, got %d", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the long poll did not return within the wait: %s", elapsed)
	}
	if got := h.poll(); got != 0 {
		t.Fatalf("the second poll invented work: %d", got)
	}
}

// TestAnUnacceptableSpecIsRefusedBeforeAnythingIsSpent. Before the negative ack
// existed, the only way to say "I cannot run this" was to create the Job, let
// it fail and report it — which costs a pod, a scheduling round and an entry in
// somebody's failure dashboard for a value that was wrong on arrival.
func TestAnUnacceptableSpecIsRefusedBeforeAnythingIsSpent(t *testing.T) {
	h := newHarness(t)
	spec := sampleSpec()
	spec.Runtime.Resources.Memory = "four gigabytes"
	id := h.Backend.Enqueue(spec)

	h.poll()

	if _, err := h.runErr(id); err == nil {
		t.Fatal("an AgentRun was created for a spec the cluster cannot materialise")
	}
	state, _ := h.Backend.RunState(id)
	if state.Status == clusterv1.StatusDispatched {
		t.Fatal("the control plane believes the run was dispatched")
	}
	if len(state.Excluded) == 0 {
		t.Fatal("the cluster was not excluded from selection for this run")
	}
}

// TestTheBundleIsReissuedBeforeTheJobStarts. An expired signature is the
// failure that looks like nothing else: the agent works for an hour, succeeds,
// and the upload fails with 403 on a link that lapsed ten minutes earlier. The
// work is gone and the logs say "storage error".
func TestTheBundleIsReissuedBeforeTheJobStarts(t *testing.T) {
	// Object-store mode: this whole failure only exists there. In relay mode
	// there is no signature to expire — the pod posts to a Service — which is
	// one of the quieter reasons the default is what it is.
	h := newHarness(t, withObjectStore())
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()

	before := bundleOf(t, h, id)

	// Far enough in that the bundle can no longer outlive an hour-long run.
	h.Clock.Advance(90 * time.Minute)
	h.reconcile(id)

	after := bundleOf(t, h, id)
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Fatalf("the bundle was not reissued: still expires at %s", after.ExpiresAt)
	}
	if h.job(id, 1) == nil {
		t.Fatal("the Job was not created after the bundle was refreshed")
	}
}

func bundleOf(t *testing.T, h *harness, id runv1.ULID) clusterv1.ArtifactBundle {
	t.Helper()
	secret := h.secret(id)
	if secret == nil {
		t.Fatal("no per-run Secret")
	}
	var bundle clusterv1.ArtifactBundle
	if err := json.Unmarshal(secret.Data[runv1.SecretKeyPresigned], &bundle); err != nil {
		t.Fatalf("decode the presigned bundle: %v", err)
	}
	return bundle
}

// TestAnOutageDoesNotStopTheWork is principle P4, stated as a test. The control
// plane going away must not stop a run that has already been paid for; the
// reports wait, and the run finishes.
func TestAnOutageDoesNotStopTheWork(t *testing.T) {
	h := newHarness(t)
	id := h.Backend.Enqueue(sampleSpec())
	h.poll()
	h.reconcile(id)

	h.Backend.SetUnavailable(true)

	pod := h.startPod(id, 1)
	h.podRunning(pod)
	h.reconcile(id)
	h.podExited(pod, 0, "Completed")
	h.reconcile(id)

	if cr := h.run(id); cr.Status.Phase != runv1.PhaseSucceeded {
		t.Fatalf("the run did not finish while the control plane was down: %s", cr.Status.Phase)
	}
	if err := h.Reporter.Flush(h.ctx); err == nil {
		t.Fatal("delivery to an unavailable control plane reported success")
	}

	h.Backend.SetUnavailable(false)
	h.flush()

	state, _ := h.Backend.RunState(id)
	if state.TerminalPhase != runv1.PhaseSucceeded {
		t.Fatalf("the outcome did not arrive after the outage: %s (%s)", state.TerminalPhase, state.Status)
	}
	// The pod never called back in this test, so the control plane knows the
	// run finished and does not yet know what it produced. That is a
	// reconciliation task — the result is in storage — and not a failure.
	if state.Status != clusterv1.StatusCompletedWithoutResult {
		t.Fatalf("want CompletedWithoutResult for a terminal run with no completion, got %s", state.Status)
	}
}
