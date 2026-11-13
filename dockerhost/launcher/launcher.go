// Package launcher turns one attempt of a run into the container that executes
// it. It is a pure function of the row, the attempt and the host settings, so
// every value in docker-host.md §5 is testable without a daemon.
//
// The environment is not rebuilt here. It is taken from controller/launcher —
// the Kubernetes builder — by building the Job for the same attempt and reading
// its container's variables. Two runtimes that each derived the variables from
// the spec would be two places the contract could drift; one function used by
// both is none. The only variable that changes form is the prompt, which on
// Kubernetes is a secretKeyRef and here is a literal (docker-host.md §5.1).
package launcher

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentrunv1alpha1 "github.com/automagicops/haliphron/api/agentrun/v1alpha1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"

	k8slauncher "github.com/automagicops/haliphron/controller/launcher"

	"github.com/automagicops/haliphron/dockerhost/docker"
	"github.com/automagicops/haliphron/dockerhost/state"
)

// Labels every object the controller creates carries. managed-by is what the
// events filter, the garbage collector and an operator's docker ps select on.
const (
	LabelManagedBy = "haliphron.io/managed-by"
	ManagedBy      = "haliphron-dockerhost"
	LabelRunID     = agentrunv1alpha1.LabelRunID
	LabelEpoch     = agentrunv1alpha1.LabelEpoch
	LabelAttempt   = agentrunv1alpha1.LabelAttempt
	LabelClusterID = agentrunv1alpha1.LabelClusterID
	LabelAgent     = agentrunv1alpha1.LabelAgent
	// LabelRole marks the containers that are not agents: the network probe.
	LabelRole = "haliphron.io/role"
	RoleAgent = "agent"
	RoleProbe = "netcheck"
)

const (
	agentUser = "1000:1000"
	// The image's directories. A fresh named volume mounted over one starts as
	// a copy of it, owned by uid 1000 as the image owns it — which is how the
	// four writable paths are writable without the controller chowning
	// anything.
	mountTmp = "/tmp"
)

// Host is what the host contributes to every agent container.
type Host struct {
	ClusterID            runv1.ULID
	CallbackURL          string
	Network              string
	DNS                  []string
	GraceSeconds         int64
	DeadlineSlackSeconds int64
	PidsLimit            int64
	Nofile               int64
	LogDriver            string
	LogMaxSize           string
	LogMaxFile           string
	// AttemptDir is the host directory holding this attempt's secrets/ and
	// role/ subdirectories.
	AttemptDir func(runID runv1.ULID, epoch int64, attempt int32) string
}

// ContainerName is deterministic, so a create that answers 409 is the
// controller's own earlier create and can be adopted.
func ContainerName(runID runv1.ULID, epoch int64, attempt int32) string {
	return fmt.Sprintf("haliphron-%s-%d-a%d", strings.ToLower(string(runID)), epoch, attempt)
}

// VolumeName is the named volume behind one of the four writable paths.
func VolumeName(runID runv1.ULID, epoch int64, attempt int32, which string) string {
	return ContainerName(runID, epoch, attempt) + "-" + which
}

// Volume suffixes, in mount order.
var volumes = []struct{ suffix, target string }{
	{"workspace", runv1.MountWorkspace},
	{"run", runv1.DirRunPrivate},
	{"home", runv1.HomeDir},
	{"tmp", mountTmp},
}

// VolumeNames lists the four volumes of one attempt.
func VolumeNames(runID runv1.ULID, epoch int64, attempt int32) []string {
	out := make([]string, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, VolumeName(runID, epoch, attempt, v.suffix))
	}
	return out
}

// Labels is the label set of one attempt's container and volumes.
func Labels(r *state.Run, attempt int32, clusterID runv1.ULID) map[string]string {
	return map[string]string{
		LabelManagedBy: ManagedBy,
		LabelRole:      RoleAgent,
		LabelRunID:     strings.ToLower(string(r.RunID)),
		LabelEpoch:     strconv.FormatInt(r.Epoch, 10),
		LabelAttempt:   strconv.Itoa(int(attempt)),
		LabelClusterID: strings.ToLower(string(clusterID)),
		LabelAgent:     string(r.Spec.Agent),
	}
}

// Limits are the parsed resource limits of a spec.
type Limits struct {
	NanoCPUs         int64
	MemoryBytes      int64
	EphemeralStorage int64
}

// ParseLimits reads the spec's quantities with Kubernetes' own parser, so a
// value one runtime accepts the other does not refuse. A path is named on
// failure because it travels back in a negative ack.
func ParseLimits(r runv1.Resources) (Limits, error) {
	reqs, err := k8slauncher.Resources(r)
	if err != nil {
		return Limits{}, err
	}
	var l Limits
	if q, ok := reqs.Limits[corev1.ResourceCPU]; ok {
		l.NanoCPUs = q.MilliValue() * 1_000_000
	}
	if q, ok := reqs.Limits[corev1.ResourceMemory]; ok {
		l.MemoryBytes = q.Value()
	}
	if q, ok := reqs.Limits[corev1.ResourceEphemeralStorage]; ok {
		l.EphemeralStorage = q.Value()
	}
	return l, nil
}

// Env is the container's environment for one attempt, as KEY=value strings in
// the order the Kubernetes builder produces them, with the prompt last.
func Env(r *state.Run, attempt int32, completedPhases []runv1.RuntimePhase, h Host) ([]string, error) {
	vars, err := envVars(r, attempt, completedPhases, h)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(vars))
	for _, v := range vars {
		out = append(out, v.Name+"="+v.Value)
	}
	return out, nil
}

// envVars builds the Kubernetes Job for the same attempt and takes its
// variables, replacing the prompt's secretKeyRef with the prompt itself.
func envVars(r *state.Run, attempt int32, completedPhases []runv1.RuntimePhase, h Host) ([]corev1.EnvVar, error) {
	cr := AgentRunFor(r, h.CallbackURL)
	builder := k8slauncher.Builder{
		ClusterID:            h.ClusterID,
		GraceSeconds:         h.GraceSeconds,
		DeadlineSlackSeconds: h.DeadlineSlackSeconds,
	}
	job, err := builder.Job(cr, attempt, completedPhases)
	if err != nil {
		return nil, err
	}
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		return nil, errors.New("launcher: the Kubernetes builder produced no single container")
	}
	out := make([]corev1.EnvVar, 0, len(containers[0].Env))
	for _, e := range containers[0].Env {
		if e.ValueFrom != nil {
			if e.Name != runv1.EnvPrompt {
				// The builder grew a second reference variable. Refusing is
				// better than starting a container without it.
				return nil, fmt.Errorf("launcher: variable %s has no literal value", e.Name)
			}
			e = corev1.EnvVar{Name: runv1.EnvPrompt, Value: r.Materials.Prompt}
		}
		out = append(out, e)
	}
	return out, nil
}

// AgentRunFor is the AgentRun the Kubernetes controller would hold for this
// row. It exists only in memory, as the builder's input.
func AgentRunFor(r *state.Run, callbackURL string) *agentrunv1alpha1.AgentRun {
	materials := agentrunv1alpha1.MaterialsRef{SecretName: agentrunv1alpha1.SecretName(r.RunID)}
	if len(r.Materials.RoleConfig) > 0 {
		materials.ConfigMapName = agentrunv1alpha1.ConfigMapName(r.RunID)
	}
	return &agentrunv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: agentrunv1alpha1.ObjectName(r.RunID)},
		Spec: agentrunv1alpha1.AgentRunSpec{
			RunID:            r.RunID,
			LeaseEpoch:       r.Epoch,
			RenderedRunSpec:  r.Spec,
			Materials:        materials,
			CallbackURL:      callbackURL,
			ArtifactMode:     r.ArtifactMode(),
			MaxArtifactBytes: r.Materials.Artifacts.MaxBytesPerRun,
		},
	}
}

// Container is the create request for one attempt.
func Container(r *state.Run, attempt int32, completedPhases []runv1.RuntimePhase, h Host) (docker.ContainerSpec, error) {
	if attempt < 1 {
		return docker.ContainerSpec{}, fmt.Errorf("launcher: attempt %d is not a counting number", attempt)
	}
	limits, err := ParseLimits(r.Spec.Runtime.Resources)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	env, err := Env(r, attempt, completedPhases, h)
	if err != nil {
		return docker.ContainerSpec{}, err
	}
	labels := Labels(r, attempt, h.ClusterID)

	grace := int(h.GraceSeconds)
	if grace <= 0 {
		grace = int(k8slauncher.DefaultGraceSeconds)
	}
	pids := h.PidsLimit
	if pids <= 0 {
		pids = 4096
	}

	mounts := make([]docker.Mount, 0, len(volumes)+2)
	for _, v := range volumes {
		mounts = append(mounts, docker.Mount{
			Type:   "volume",
			Source: VolumeName(r.RunID, r.Epoch, attempt, v.suffix),
			Target: v.target,
			VolumeOptions: &docker.VolumeOptions{
				NoCopy: false,
				Labels: labels,
			},
		})
	}
	dir := h.AttemptDir(r.RunID, r.Epoch, attempt)
	mounts = append(mounts, docker.Mount{
		Type: "bind", Source: filepath.Join(dir, "secrets"), Target: runv1.MountSecrets, ReadOnly: true,
		BindOptions: &docker.BindOptions{Propagation: "rprivate"},
	})
	if len(r.Materials.RoleConfig) > 0 {
		mounts = append(mounts, docker.Mount{
			Type: "bind", Source: filepath.Join(dir, "role"), Target: runv1.MountRoleConfig, ReadOnly: true,
			BindOptions: &docker.BindOptions{Propagation: "rprivate"},
		})
	}

	logCfg := docker.LogConfig{Type: h.LogDriver}
	switch h.LogDriver {
	case "", "local", "json-file":
		if logCfg.Type == "" {
			logCfg.Type = "local"
		}
		logCfg.Config = map[string]string{"max-size": orDefault(h.LogMaxSize, "10m"), "max-file": orDefault(h.LogMaxFile, "2")}
	}

	hc := docker.HostConfig{
		NetworkMode:    h.Network,
		DNS:            append([]string(nil), h.DNS...),
		Mounts:         mounts,
		ReadonlyRootfs: true,
		CapDrop:        []string{"ALL"},
		// seccomp and AppArmor stay at the daemon's defaults, which is what
		// RuntimeDefault means on Kubernetes.
		SecurityOpt:   []string{"no-new-privileges:true"},
		Privileged:    false,
		IpcMode:       "private",
		PidMode:       "",
		RestartPolicy: docker.RestartPolicy{Name: "no"},
		AutoRemove:    false,
		NanoCPUs:      limits.NanoCPUs,
		Memory:        limits.MemoryBytes,
		PidsLimit:     &pids,
		LogConfig:     logCfg,
	}
	if limits.MemoryBytes > 0 {
		// No swap: an agent that pages is an agent that is an hour slower and
		// still bills for the model by the token.
		hc.MemorySwap = limits.MemoryBytes
	}
	if h.Nofile > 0 {
		hc.Ulimits = []docker.Ulimit{{Name: "nofile", Soft: h.Nofile, Hard: h.Nofile}}
	}

	return docker.ContainerSpec{
		Image:       r.Spec.Image,
		User:        agentUser,
		Env:         env,
		Labels:      labels,
		StopSignal:  "SIGTERM",
		StopTimeout: &grace,
		HostConfig:  hc,
		NetworkingConfig: &docker.NetworkingConfig{
			EndpointsConfig: map[string]docker.EndpointSettings{h.Network: {}},
		},
	}, nil
}

// Files is what the attempt directory holds: the per-run Secret's keys under
// secrets/, mode 0400, and the role's files under role/, mode 0444. The
// mapping is the Kubernetes controller's ensureMaterials: every lease secret,
// the prompt, the callback token, and the presigned bundle in object-store
// mode.
type Files struct {
	Secrets map[string][]byte
	Role    map[string][]byte
}

// MaterialFiles derives the attempt's files from the row.
func MaterialFiles(r *state.Run, presigned []byte) Files {
	f := Files{Secrets: map[string][]byte{}, Role: map[string][]byte{}}
	for k, v := range r.Materials.Secrets {
		f.Secrets[k] = []byte(v)
	}
	f.Secrets[runv1.SecretKeyPrompt] = []byte(r.Materials.Prompt)
	f.Secrets[runv1.SecretKeyCallbackToken] = []byte(r.CallbackToken)
	if r.ArtifactMode() == runv1.ArtifactModeObjectStore && len(presigned) > 0 {
		f.Secrets[runv1.SecretKeyPresigned] = presigned
	}
	for k, v := range r.Materials.RoleConfig {
		f.Role[k] = []byte(v)
	}
	return f
}

// SortedKeys is for deterministic writes and tests.
func SortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
