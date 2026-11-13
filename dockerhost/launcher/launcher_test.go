package launcher

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"

	k8slauncher "github.com/automagicops/haliphron/controller/launcher"

	"github.com/automagicops/haliphron/dockerhost/state"
)

func testHost() Host {
	return Host{
		ClusterID:            "01HZCLUSTER000000000000000",
		CallbackURL:          "http://172.31.250.1:8083",
		Network:              "haliphron-agents",
		DNS:                  []string{"10.0.0.53"},
		GraceSeconds:         45,
		DeadlineSlackSeconds: 300,
		PidsLimit:            2048,
		Nofile:               4096,
		LogDriver:            "local",
		AttemptDir: func(runID runv1.ULID, epoch int64, attempt int32) string {
			return "/run/haliphron/runs/" + strings.ToLower(string(runID)) + "/a"
		},
	}
}

func testRow() *state.Run {
	createPR := true
	chunk := int32(15)
	return &state.Run{
		RunID: "01HZRUN0000000000000000000",
		Epoch: 3,
		Spec: runv1.RenderedRunSpec{
			Agent:        "claude-code",
			PromptSHA256: "abc",
			Model:        "claude-opus-5-5",
			Role:         "reviewer",
			Image:        "javdet/haliphron-agent:0.6.2",
			Repo: runv1.RepoSpec{
				URL: "https://git.example.com/a/b.git", Provider: "gitlab",
				BaseBranch: "main", TargetBranch: "haliphron/x", CreatePR: &createPR, CloneDepth: 5,
			},
			Runtime: runv1.RuntimeSpec{
				TimeoutSeconds: 1800, MaxTurns: 40, PermissionMode: "acceptEdits",
				Env:       []runv1.EnvVar{{Name: "EXTRA", Value: "1"}},
				Resources: runv1.Resources{CPU: "1500m", Memory: "3Gi", EphemeralStorage: "8Gi"},
			},
			ToolPolicy:    &runv1.ToolPolicy{Allow: []string{"Read", "Edit"}, Deny: []string{"Bash"}},
			Observability: &runv1.ObservabilitySpec{Traceparent: "00-a-b-01", LogChunkIntervalSeconds: &chunk},
		},
		CallbackToken: "tok",
		Materials: state.Materials{
			Prompt:     "do the thing",
			Secrets:    map[string]string{runv1.SecretKeyGitToken: "ghp", runv1.SecretKeyLLMAPIKey: "sk"},
			RoleConfig: map[string]string{runv1.RoleConfigKeySystemPrompt: "be terse"},
		},
	}
}

func TestTheEnvironmentIsTheKubernetesBuildersWithThePromptAsALiteral(t *testing.T) {
	row := testRow()
	h := testHost()
	phases := []runv1.RuntimePhase{runv1.RuntimePhaseClone, runv1.RuntimePhaseInit}

	env, err := Env(row, 2, phases, h)
	if err != nil {
		t.Fatal(err)
	}
	job, err := k8slauncher.Builder{ClusterID: h.ClusterID, GraceSeconds: h.GraceSeconds,
		DeadlineSlackSeconds: h.DeadlineSlackSeconds}.Job(AgentRunFor(row, h.CallbackURL), 2, phases)
	if err != nil {
		t.Fatal(err)
	}
	want := job.Spec.Template.Spec.Containers[0].Env
	if len(env) != len(want) {
		t.Fatalf("got %d variables, the Kubernetes builder produces %d", len(env), len(want))
	}
	for i, w := range want {
		name, value, _ := strings.Cut(env[i], "=")
		if name != w.Name {
			t.Fatalf("variable %d is %s, the Kubernetes builder has %s", i, name, w.Name)
		}
		if w.ValueFrom != nil {
			if name != runv1.EnvPrompt || value != row.Materials.Prompt {
				t.Fatalf("%s should carry the prompt as a literal, got %q", name, value)
			}
			continue
		}
		if value != w.Value {
			t.Fatalf("%s=%q, the Kubernetes builder has %q", name, value, w.Value)
		}
	}
}

func TestEveryVariableIsInTheContractAndTheRequiredOnesArePresent(t *testing.T) {
	env, err := Env(testRow(), 1, nil, testHost())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for name := range got {
		if name == "EXTRA" {
			continue // the spec's free-form extras
		}
		if !slices.Contains(runv1.ContractEnv, name) {
			t.Errorf("%s is not a variable of the runtime contract", name)
		}
	}
	for _, required := range []string{
		runv1.EnvContract, runv1.EnvRunID, runv1.EnvAttempt, runv1.EnvClusterID, runv1.EnvCallbackURL,
		runv1.EnvGraceSeconds, runv1.EnvAgent, runv1.EnvModel, runv1.EnvTimeoutSeconds,
		runv1.EnvPrompt, runv1.EnvPromptSHA256, runv1.EnvArtifactMode, runv1.EnvStoragePrefix,
		runv1.EnvCreatePR,
	} {
		if _, ok := got[required]; !ok {
			t.Errorf("%s is missing", required)
		}
	}
	if got[runv1.EnvCallbackURL] != "http://172.31.250.1:8083" {
		t.Errorf("callback URL is %q", got[runv1.EnvCallbackURL])
	}
	if got[runv1.EnvArtifactMode] != string(runv1.ArtifactModeRelay) {
		t.Errorf("an unset mode must read as relay, got %q", got[runv1.EnvArtifactMode])
	}
}

func TestTheContainerIsHardenedAsTheContractRequires(t *testing.T) {
	row := testRow()
	spec, err := Container(row, 2, nil, testHost())
	if err != nil {
		t.Fatal(err)
	}
	hc := spec.HostConfig
	switch {
	case spec.User != "1000:1000":
		t.Errorf("user %q", spec.User)
	case !hc.ReadonlyRootfs:
		t.Error("the root filesystem must be read-only")
	case !slices.Equal(hc.CapDrop, []string{"ALL"}):
		t.Errorf("capabilities dropped: %v", hc.CapDrop)
	case !slices.Contains(hc.SecurityOpt, "no-new-privileges:true"):
		t.Errorf("security options: %v", hc.SecurityOpt)
	case hc.Privileged:
		t.Error("privileged")
	case hc.RestartPolicy.Name != "no":
		t.Errorf("restart policy %q: every attempt is the controller's decision", hc.RestartPolicy.Name)
	case hc.AutoRemove:
		t.Error("auto-remove would lose the exit code")
	case hc.NanoCPUs != 1_500_000_000:
		t.Errorf("nanocpus %d", hc.NanoCPUs)
	case hc.Memory != 3<<30 || hc.MemorySwap != hc.Memory:
		t.Errorf("memory %d swap %d: swap must equal memory", hc.Memory, hc.MemorySwap)
	case hc.PidsLimit == nil || *hc.PidsLimit != 2048:
		t.Errorf("pids limit %v", hc.PidsLimit)
	case hc.NetworkMode != "haliphron-agents" || !slices.Equal(hc.DNS, []string{"10.0.0.53"}):
		t.Errorf("network %q dns %v", hc.NetworkMode, hc.DNS)
	case len(hc.PortBindings) != 0:
		t.Error("an agent publishes no ports")
	case spec.StopTimeout == nil || *spec.StopTimeout != 45:
		t.Errorf("stop timeout %v", spec.StopTimeout)
	case spec.Labels[LabelManagedBy] != ManagedBy || spec.Labels[LabelAttempt] != "2" || spec.Labels[LabelEpoch] != "3":
		t.Errorf("labels %v", spec.Labels)
	}

	targets := map[string]bool{}
	for _, m := range hc.Mounts {
		targets[m.Target] = true
		switch m.Target {
		case runv1.MountSecrets, runv1.MountRoleConfig:
			if m.Type != "bind" || !m.ReadOnly {
				t.Errorf("%s must be a read-only bind mount, got %+v", m.Target, m)
			}
		default:
			if m.Type != "volume" || m.ReadOnly || m.VolumeOptions == nil || m.VolumeOptions.NoCopy {
				t.Errorf("%s must be a writable named volume initialised from the image, got %+v", m.Target, m)
			}
			if !strings.HasPrefix(m.Source, "haliphron-01hzrun0000000000000000000-3-a2-") {
				t.Errorf("volume %s is not named after the attempt", m.Source)
			}
		}
	}
	for _, want := range []string{runv1.MountWorkspace, runv1.DirRunPrivate, runv1.HomeDir, "/tmp", runv1.MountSecrets, runv1.MountRoleConfig} {
		if !targets[want] {
			t.Errorf("no mount at %s", want)
		}
	}
	if len(hc.Mounts) != 6 {
		t.Errorf("%d mounts: the four writable paths and the two read-only ones, nothing else", len(hc.Mounts))
	}
}

func TestNoRoleMountWithoutRoleFiles(t *testing.T) {
	row := testRow()
	row.Materials.RoleConfig = nil
	spec, err := Container(row, 1, nil, testHost())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range spec.HostConfig.Mounts {
		if m.Target == runv1.MountRoleConfig {
			t.Fatal("a role mount with no role files")
		}
	}
}

func TestAnUnparsableQuantityNamesItsPath(t *testing.T) {
	row := testRow()
	row.Spec.Runtime.Resources.Memory = "lots"
	_, err := Container(row, 1, nil, testHost())
	var invalid *k8slauncher.InvalidFieldError
	if err == nil || !errorsAs(err, &invalid) || invalid.Path != "runtime.resources.memory" {
		t.Fatalf("got %v", err)
	}
}

func TestTheMaterialFilesAreTheKubernetesSecretsKeys(t *testing.T) {
	row := testRow()
	f := MaterialFiles(row, []byte(`{"mode":"object-store"}`))
	for _, k := range []string{runv1.SecretKeyGitToken, runv1.SecretKeyLLMAPIKey, runv1.SecretKeyPrompt, runv1.SecretKeyCallbackToken} {
		if _, ok := f.Secrets[k]; !ok {
			t.Errorf("no %s", k)
		}
	}
	if _, ok := f.Secrets[runv1.SecretKeyPresigned]; ok {
		t.Error("relay mode has no presigned bundle")
	}
	row.Materials.Artifacts.Mode = runv1.ArtifactModeObjectStore
	if _, ok := MaterialFiles(row, []byte("{}")).Secrets[runv1.SecretKeyPresigned]; !ok {
		t.Error("object-store mode needs the presigned bundle")
	}
}

func TestSecretFilesAreReadOnlyToTheirOwnerAndBadKeysAreRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a1")
	f := Files{Secrets: map[string][]byte{"git-token": []byte("x")}, Role: map[string][]byte{"plugins.json": []byte("{}")}}
	if err := WriteFiles(dir, f, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "secrets", "git-token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Errorf("secret file mode %v", info.Mode().Perm())
	}
	info, _ = os.Stat(filepath.Join(dir, "role", "plugins.json"))
	if info.Mode().Perm() != 0o444 {
		t.Errorf("role file mode %v", info.Mode().Perm())
	}
	if err := RemoveFiles(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the attempt directory survived its removal")
	}
	bad := Files{Secrets: map[string][]byte{"../escape": []byte("x")}}
	if err := WriteFiles(dir, bad, false); err == nil {
		t.Error("a key with a path separator was written")
	}
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }
