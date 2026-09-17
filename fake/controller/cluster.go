package controller

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// objectStore is the cluster's API server reduced to the four kinds this
// contract touches. It models the two behaviours the CRD contract cares about
// and nothing else:
//
//   - pruning: an unknown spec field is deleted on write and the write
//     succeeds, which is the trap the whole read-back mechanism exists for;
//   - ownership: deleting an AgentRun collects the Secret, ConfigMap and Job
//     that reference it, which is what makes "no long-lived secret is left in
//     the agents namespace" true.
//
// Everything else a real API server does — admission, scheduling, the Job
// controller — is absent on purpose. This fake never claims a Job ran.
type objectStore struct {
	mu sync.Mutex

	agentRuns  map[string]*agentrunv1alpha1.AgentRun
	secrets    map[string]*corev1.Secret
	configMaps map[string]*corev1.ConfigMap
	jobs       map[string]*batchv1.Job

	// prune are the dotted spec paths this cluster's CRD does not know.
	prune []string
}

func newObjectStore() *objectStore {
	return &objectStore{
		agentRuns:  map[string]*agentrunv1alpha1.AgentRun{},
		secrets:    map[string]*corev1.Secret{},
		configMaps: map[string]*corev1.ConfigMap{},
		jobs:       map[string]*batchv1.Job{},
	}
}

// createAgentRun writes the CR, applying the cluster's pruning on the way in,
// and returns what a read-back would see. The caller compares that against the
// hash it recorded before the write: the API server answers 201 either way, so
// the divergence is the only evidence that anything was lost.
func (s *objectStore) createAgentRun(cr *agentrunv1alpha1.AgentRun) *agentrunv1alpha1.AgentRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cr.DeepCopy()
	if len(s.prune) > 0 {
		stored.Spec.RenderedRunSpec = pruneSpec(stored.Spec.RenderedRunSpec, s.prune)
	}
	s.agentRuns[stored.Name] = stored
	return stored.DeepCopy()
}

func (s *objectStore) getAgentRun(name string) (*agentrunv1alpha1.AgentRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok := s.agentRuns[name]
	if !ok {
		return nil, false
	}
	return cr.DeepCopy(), true
}

// replaceStatus writes an object back without re-applying pruning. The status
// subresource is a separate write in a real cluster, and it cannot smuggle in a
// spec change — which is what makes it safe to grant the controller.
func (s *objectStore) replaceStatus(cr *agentrunv1alpha1.AgentRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.agentRuns[cr.Name]
	if !ok {
		return
	}
	updated := cr.DeepCopy()
	updated.Spec = existing.Spec
	s.agentRuns[cr.Name] = updated
}

func (s *objectStore) putSecret(o *corev1.Secret) { s.mu.Lock(); s.secrets[o.Name] = o; s.mu.Unlock() }
func (s *objectStore) putConfigMap(o *corev1.ConfigMap) {
	s.mu.Lock()
	s.configMaps[o.Name] = o
	s.mu.Unlock()
}
func (s *objectStore) putJob(o *batchv1.Job) { s.mu.Lock(); s.jobs[o.Name] = o; s.mu.Unlock() }

func (s *objectStore) deleteJob(name string) {
	s.mu.Lock()
	delete(s.jobs, name)
	s.mu.Unlock()
}

// deleteAgentRun removes the CR and everything owned by it. Garbage collection
// by ownerReference is not a detail here: it is the mechanism that guarantees
// the per-run Secret does not outlive the run.
func (s *objectStore) deleteAgentRun(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok := s.agentRuns[name]
	if !ok {
		return
	}
	uid := string(cr.UID)
	delete(s.agentRuns, name)
	for k, o := range s.secrets {
		if ownedBy(o.OwnerReferences, uid) {
			delete(s.secrets, k)
		}
	}
	for k, o := range s.configMaps {
		if ownedBy(o.OwnerReferences, uid) {
			delete(s.configMaps, k)
		}
	}
	for k, o := range s.jobs {
		if ownedBy(o.OwnerReferences, uid) {
			delete(s.jobs, k)
		}
	}
}

func ownedBy(refs []metav1.OwnerReference, uid string) bool {
	for _, ref := range refs {
		if string(ref.UID) == uid {
			return true
		}
	}
	return false
}

// Snapshot is the cluster's contents, copied. The backend's tests assert on
// this: it is the only view they have of what the controller actually built.
type Snapshot struct {
	AgentRuns  []agentrunv1alpha1.AgentRun
	Secrets    []corev1.Secret
	ConfigMaps []corev1.ConfigMap
	Jobs       []batchv1.Job
}

// Objects returns a snapshot of the cluster.
func (c *Controller) Objects() Snapshot {
	return c.objects.snapshot()
}

func (s *objectStore) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Snapshot
	for _, o := range s.agentRuns {
		out.AgentRuns = append(out.AgentRuns, *o.DeepCopy())
	}
	for _, o := range s.secrets {
		out.Secrets = append(out.Secrets, *o.DeepCopy())
	}
	for _, o := range s.configMaps {
		out.ConfigMaps = append(out.ConfigMaps, *o.DeepCopy())
	}
	for _, o := range s.jobs {
		out.Jobs = append(out.Jobs, *o.DeepCopy())
	}
	sort.Slice(out.AgentRuns, func(i, j int) bool { return out.AgentRuns[i].Name < out.AgentRuns[j].Name })
	sort.Slice(out.Secrets, func(i, j int) bool { return out.Secrets[i].Name < out.Secrets[j].Name })
	sort.Slice(out.ConfigMaps, func(i, j int) bool { return out.ConfigMaps[i].Name < out.ConfigMaps[j].Name })
	sort.Slice(out.Jobs, func(i, j int) bool { return out.Jobs[i].Name < out.Jobs[j].Name })
	return out
}

// AgentRun returns the CR for a run, if the cluster holds one.
func (c *Controller) AgentRun(id runv1.ULID) (*agentrunv1alpha1.AgentRun, bool) {
	return c.objects.getAgentRun(agentrunv1alpha1.ObjectName(id))
}

// pruneSpec deletes the named dotted paths, the way a structural schema drops
// fields it has never heard of. Round-tripping through a map is not a shortcut:
// it is what the API server effectively does, and doing it any other way would
// prune fields the Go type cannot represent as absent.
func pruneSpec(spec runv1.RenderedRunSpec, paths []string) runv1.RenderedRunSpec {
	raw, err := json.Marshal(spec)
	if err != nil {
		return spec
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return spec
	}
	for _, path := range paths {
		deletePath(generic, strings.Split(path, "."))
	}
	out, err := json.Marshal(generic)
	if err != nil {
		return spec
	}
	var pruned runv1.RenderedRunSpec
	if err := json.Unmarshal(out, &pruned); err != nil {
		return spec
	}
	return pruned
}

func deletePath(node map[string]any, path []string) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		delete(node, path[0])
		return
	}
	if child, ok := node[path[0]].(map[string]any); ok {
		deletePath(child, path[1:])
	}
}
