// Package docker is the narrow part of the Docker Engine API the controller
// uses, as an interface and one implementation over the daemon's socket.
//
// It is hand-written rather than the official client for two reasons. The
// official module pulls in a dependency graph larger than the rest of this
// controller, for the sake of a dozen endpoints. And an interface this small is
// one the contract suite can fake faithfully: every method here has a fake in
// test/dockerhost, and a method nobody fakes is a method nobody tests.
//
// The types below are subsets of the Engine API's JSON, named as the API names
// them, holding only the fields the controller reads or sets. The API version
// is pinned (APIVersion): Docker Engine 25 and later speak it, and a pinned
// version is what makes the daemon's answers mean the same thing on every host.
package docker

import (
	"context"
	"errors"
	"time"
)

// APIVersion is the Engine API version every request names. 1.44 is Docker
// Engine 25.0, the supported baseline.
const APIVersion = "1.44"

// ErrNotFound is a 404 from the daemon: no such container, image, volume or
// network. Callers branch on it, so it is a sentinel and not a status code.
var ErrNotFound = errors.New("docker: not found")

// ErrConflict is a 409: a container with that name already exists, or a volume
// is in use.
var ErrConflict = errors.New("docker: conflict")

// Engine is everything the controller asks of the daemon.
type Engine interface {
	Info(ctx context.Context) (Info, error)
	Version(ctx context.Context) (VersionInfo, error)

	ImageInspect(ctx context.Context, ref string) (Image, error)
	// ImagePull pulls one reference and returns when the pull has finished or
	// failed. auth is the base64 X-Registry-Auth value, or empty.
	ImagePull(ctx context.Context, ref, auth string) error
	ImageRemove(ctx context.Context, ref string) error

	ContainerCreate(ctx context.Context, name string, spec ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	// ContainerKill sends a signal and returns at once. Stopping is the
	// controller's own sequence — SIGTERM, a recorded deadline, SIGKILL — so
	// that no call ever blocks a reconcile for a grace period.
	ContainerKill(ctx context.Context, id, signal string) error
	ContainerInspect(ctx context.Context, idOrName string) (Container, error)
	ContainerRemove(ctx context.Context, id string, force bool) error
	ContainerList(ctx context.Context, labels map[string]string) ([]ContainerSummary, error)
	ContainerLogs(ctx context.Context, id string) (string, error)

	VolumeRemove(ctx context.Context, name string, force bool) error
	VolumeList(ctx context.Context, labels map[string]string) ([]Volume, error)

	NetworkInspect(ctx context.Context, name string) (Network, error)

	// Events streams container events matching the labels until the context
	// ends or the stream breaks. The error channel receives exactly one value
	// when the stream ends. Events are a hint: the resync is what guarantees
	// convergence, so a lost event costs latency and nothing else.
	Events(ctx context.Context, labels map[string]string) (<-chan Event, <-chan error)
}

// Info is the part of GET /info the controller reads.
type Info struct {
	ID              string `json:"ID"`
	NCPU            int    `json:"NCPU"`
	MemTotal        int64  `json:"MemTotal"`
	ServerVersion   string `json:"ServerVersion"`
	DockerRootDir   string `json:"DockerRootDir"`
	OperatingSystem string `json:"OperatingSystem"`
}

// VersionInfo is GET /version.
type VersionInfo struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// Image is the part of GET /images/{name}/json the controller reads.
type Image struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
}

// ContainerSpec is the body of POST /containers/create: the container config
// with the host config embedded, as the API takes it.
type ContainerSpec struct {
	Image        string            `json:"Image"`
	User         string            `json:"User,omitempty"`
	Env          []string          `json:"Env,omitempty"`
	Cmd          []string          `json:"Cmd,omitempty"`
	Entrypoint   []string          `json:"Entrypoint,omitempty"`
	WorkingDir   string            `json:"WorkingDir,omitempty"`
	Labels       map[string]string `json:"Labels,omitempty"`
	StopSignal   string            `json:"StopSignal,omitempty"`
	StopTimeout  *int              `json:"StopTimeout,omitempty"`
	Hostname     string            `json:"Hostname,omitempty"`
	AttachStdout bool              `json:"AttachStdout"`
	AttachStderr bool              `json:"AttachStderr"`
	Tty          bool              `json:"Tty"`

	HostConfig HostConfig `json:"HostConfig"`

	NetworkingConfig *NetworkingConfig `json:"NetworkingConfig,omitempty"`
}

// HostConfig is the half of a container's configuration that is about the
// host: resources, mounts, security.
type HostConfig struct {
	NetworkMode    string            `json:"NetworkMode,omitempty"`
	DNS            []string          `json:"Dns,omitempty"`
	Mounts         []Mount           `json:"Mounts,omitempty"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
	CapDrop        []string          `json:"CapDrop,omitempty"`
	CapAdd         []string          `json:"CapAdd,omitempty"`
	SecurityOpt    []string          `json:"SecurityOpt,omitempty"`
	Privileged     bool              `json:"Privileged"`
	IpcMode        string            `json:"IpcMode,omitempty"`
	PidMode        string            `json:"PidMode,omitempty"`
	RestartPolicy  RestartPolicy     `json:"RestartPolicy"`
	AutoRemove     bool              `json:"AutoRemove"`
	Init           *bool             `json:"Init,omitempty"`
	NanoCPUs       int64             `json:"NanoCpus,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`
	MemorySwap     int64             `json:"MemorySwap,omitempty"`
	PidsLimit      *int64            `json:"PidsLimit,omitempty"`
	Ulimits        []Ulimit          `json:"Ulimits,omitempty"`
	LogConfig      LogConfig         `json:"LogConfig"`
	PortBindings   map[string]any    `json:"PortBindings,omitempty"`
	Sysctls        map[string]string `json:"Sysctls,omitempty"`
}

// Mount is one entry of HostConfig.Mounts.
type Mount struct {
	Type          string         `json:"Type"`
	Source        string         `json:"Source"`
	Target        string         `json:"Target"`
	ReadOnly      bool           `json:"ReadOnly"`
	VolumeOptions *VolumeOptions `json:"VolumeOptions,omitempty"`
	BindOptions   *BindOptions   `json:"BindOptions,omitempty"`
}

// VolumeOptions applies to a named volume the daemon creates on first mount.
type VolumeOptions struct {
	// NoCopy false is what makes a fresh volume start as a copy of the image's
	// directory, owned as the image owns it.
	NoCopy bool              `json:"NoCopy"`
	Labels map[string]string `json:"Labels,omitempty"`
}

// BindOptions applies to a bind mount.
type BindOptions struct {
	Propagation string `json:"Propagation,omitempty"`
}

// RestartPolicy is always "no" for agents: every attempt is the controller's
// decision.
type RestartPolicy struct {
	Name string `json:"Name"`
}

// Ulimit is one resource limit.
type Ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

// LogConfig is the container's log driver.
type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

// NetworkingConfig attaches the container to networks at create time.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointSettings `json:"EndpointsConfig"`
}

// EndpointSettings is one network attachment.
type EndpointSettings struct {
	IPAddress string `json:"IPAddress,omitempty"`
	Gateway   string `json:"Gateway,omitempty"`
	NetworkID string `json:"NetworkID,omitempty"`
}

// Container is the part of GET /containers/{id}/json the controller reads.
type Container struct {
	ID      string         `json:"Id"`
	Name    string         `json:"Name"`
	Created time.Time      `json:"Created"`
	State   ContainerState `json:"State"`
	Image   string         `json:"Image"`
	Config  struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]EndpointSettings `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

// Container states, as the daemon names them.
const (
	StateCreated    = "created"
	StateRunning    = "running"
	StatePaused     = "paused"
	StateRestarting = "restarting"
	StateRemoving   = "removing"
	StateExited     = "exited"
	StateDead       = "dead"
)

// ContainerState is what the daemon knows about a container's process.
type ContainerState struct {
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	OOMKilled  bool      `json:"OOMKilled"`
	Dead       bool      `json:"Dead"`
	ExitCode   int       `json:"ExitCode"`
	Error      string    `json:"Error"`
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
}

// Started reports whether the process ever started. The daemon reports the
// zero time as 0001-01-01, which time.Time parses as zero.
func (s ContainerState) Started() bool { return !s.StartedAt.IsZero() && s.StartedAt.Year() > 1 }

// Finished reports whether the process has ended.
func (s ContainerState) Finished() bool {
	return s.Status == StateExited || s.Status == StateDead
}

// ContainerSummary is one row of GET /containers/json.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Created int64             `json:"Created"`
}

// Volume is one row of GET /volumes.
type Volume struct {
	Name      string            `json:"Name"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

// Network is the part of GET /networks/{id} the controller checks.
type Network struct {
	ID         string            `json:"Id"`
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	Internal   bool              `json:"Internal"`
	Options    map[string]string `json:"Options"`
	IPAM       struct {
		Config []struct {
			Subnet  string `json:"Subnet"`
			Gateway string `json:"Gateway"`
		} `json:"Config"`
	} `json:"IPAM"`
}

// Event is one entry of GET /events.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time int64 `json:"time"`
}
