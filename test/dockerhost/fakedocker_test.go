package dockerhost

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/automagicops/haliphron/dockerhost/docker"
)

// fakeDocker is the Engine in memory: containers, volumes, images and an
// events stream. It is faithful in the ways the controller depends on — a
// duplicate name is a 409, a missing object is a 404, a removed container's
// named volumes stay until removed by name, a signal ends a running process
// with 128+signal — and it lets the test do what a daemon does on its own:
// end a process with an exit code, OOM-kill it, restart and kill everything.
//
// It lives in the test module, so nothing that ships carries it.
type fakeDocker struct {
	mu         sync.Mutex
	seq        int
	containers map[string]*fakeContainer // by name
	volumes    map[string]map[string]string
	images     map[string]bool
	pullErr    map[string]error
	subs       []chan docker.Event
	// failStart makes the runtime refuse to start containers, the way a
	// missing mount source does.
	failStart string
}

type fakeContainer struct {
	id      string
	name    string
	spec    docker.ContainerSpec
	created time.Time
	state   docker.ContainerState
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		containers: map[string]*fakeContainer{},
		volumes:    map[string]map[string]string{},
		images:     map[string]bool{},
		pullErr:    map[string]error{},
	}
}

func (f *fakeDocker) emit(c *fakeContainer, action string) {
	ev := docker.Event{Type: "container", Action: action}
	ev.Actor.ID = c.id
	ev.Actor.Attributes = map[string]string{}
	for k, v := range c.spec.Labels {
		ev.Actor.Attributes[k] = v
	}
	for _, s := range f.subs {
		select {
		case s <- ev:
		default:
		}
	}
}

func (f *fakeDocker) find(idOrName string) *fakeContainer {
	if c, ok := f.containers[strings.TrimPrefix(idOrName, "/")]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.id == idOrName {
			return c
		}
	}
	return nil
}

func notFound(what string) error {
	return &docker.APIError{Op: "fake", Status: 404, Message: "no such " + what}
}

func conflict(what string) error {
	return &docker.APIError{Op: "fake", Status: 409, Message: what}
}

func (f *fakeDocker) Info(context.Context) (docker.Info, error) {
	return docker.Info{NCPU: 16, MemTotal: 64 << 30, ServerVersion: "27.3.1"}, nil
}

func (f *fakeDocker) Version(context.Context) (docker.VersionInfo, error) {
	return docker.VersionInfo{Version: "27.3.1", APIVersion: docker.APIVersion}, nil
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string) (docker.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[ref] {
		return docker.Image{}, notFound("image")
	}
	return docker.Image{ID: "sha256:" + ref}, nil
}

func (f *fakeDocker) ImagePull(_ context.Context, ref, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.pullErr[ref]; err != nil {
		return err
	}
	f.images[ref] = true
	return nil
}

func (f *fakeDocker) ImageRemove(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.images[ref] {
		return notFound("image")
	}
	delete(f.images, ref)
	return nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, name string, spec docker.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[name]; ok {
		return "", conflict("name in use")
	}
	if !f.images[spec.Image] {
		return "", notFound("image " + spec.Image)
	}
	f.seq++
	c := &fakeContainer{
		id: fmt.Sprintf("c%04d", f.seq), name: name, spec: spec, created: time.Now(),
		state: docker.ContainerState{Status: docker.StateCreated},
	}
	f.containers[name] = c
	for _, m := range spec.HostConfig.Mounts {
		if m.Type == "volume" {
			if _, ok := f.volumes[m.Source]; !ok {
				labels := map[string]string{}
				if m.VolumeOptions != nil {
					for k, v := range m.VolumeOptions.Labels {
						labels[k] = v
					}
				}
				f.volumes[m.Source] = labels
			}
		}
	}
	f.emit(c, "create")
	return c.id, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return notFound("container")
	}
	if c.state.Status == docker.StateRunning {
		return nil
	}
	if f.failStart != "" {
		c.state.Error = f.failStart
		c.state.ExitCode = 127
		return &docker.APIError{Op: "fake", Status: 500, Message: f.failStart}
	}
	c.state.Status = docker.StateRunning
	c.state.Running = true
	c.state.StartedAt = time.Now()
	f.emit(c, "start")
	return nil
}

func (f *fakeDocker) ContainerKill(_ context.Context, id, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return notFound("container")
	}
	if c.state.Status != docker.StateRunning {
		return nil
	}
	code := 143
	if signal == "SIGKILL" {
		code = 137
	}
	f.exitLocked(c, code, false)
	return nil
}

func (f *fakeDocker) exitLocked(c *fakeContainer, code int, oom bool) {
	c.state.Status = docker.StateExited
	c.state.Running = false
	c.state.ExitCode = code
	c.state.OOMKilled = oom
	c.state.FinishedAt = time.Now()
	if oom {
		f.emit(c, "oom")
	}
	f.emit(c, "die")
}

func (f *fakeDocker) ContainerInspect(_ context.Context, idOrName string) (docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(idOrName)
	if c == nil {
		return docker.Container{}, notFound("container")
	}
	out := docker.Container{ID: c.id, Name: "/" + c.name, Created: c.created, State: c.state, Image: c.spec.Image}
	out.Config.Image = c.spec.Image
	out.Config.Labels = c.spec.Labels
	out.NetworkSettings.Networks = map[string]docker.EndpointSettings{
		c.spec.HostConfig.NetworkMode: {IPAddress: "172.31.250.10"},
	}
	return out, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return notFound("container")
	}
	if c.state.Status == docker.StateRunning && !force {
		return conflict("container is running")
	}
	delete(f.containers, c.name)
	f.emit(c, "destroy")
	return nil
}

func matches(labels, want map[string]string) bool {
	for k, v := range want {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeDocker) ContainerList(_ context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.ContainerSummary
	for _, c := range f.containers {
		if matches(c.spec.Labels, labels) {
			out = append(out, docker.ContainerSummary{ID: c.id, Names: []string{"/" + c.name},
				Image: c.spec.Image, Labels: c.spec.Labels, State: c.state.Status, Created: c.created.Unix()})
		}
	}
	return out, nil
}

func (f *fakeDocker) ContainerLogs(context.Context, string) (string, error) { return "", nil }

func (f *fakeDocker) VolumeRemove(_ context.Context, name string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[name]; !ok {
		return notFound("volume")
	}
	for _, c := range f.containers {
		for _, m := range c.spec.HostConfig.Mounts {
			if m.Source == name {
				return conflict("volume in use")
			}
		}
	}
	delete(f.volumes, name)
	return nil
}

func (f *fakeDocker) VolumeList(_ context.Context, labels map[string]string) ([]docker.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.Volume
	for name, l := range f.volumes {
		if matches(l, labels) {
			out = append(out, docker.Volume{Name: name, Labels: l})
		}
	}
	return out, nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, name string) (docker.Network, error) {
	return docker.Network{Name: name, Driver: "bridge"}, nil
}

func (f *fakeDocker) Events(ctx context.Context, _ map[string]string) (<-chan docker.Event, <-chan error) {
	ch := make(chan docker.Event, 256)
	errs := make(chan error, 1)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	go func() {
		<-ctx.Done()
		f.mu.Lock()
		for i, s := range f.subs {
			if s == ch {
				f.subs = append(f.subs[:i], f.subs[i+1:]...)
				break
			}
		}
		f.mu.Unlock()
		close(ch)
		errs <- ctx.Err()
	}()
	return ch, errs
}

// The test's side: what a daemon and a process do without being asked.

// Exit ends a running container's process with a code.
func (f *fakeDocker) Exit(name string, code int, oom bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[name]; c != nil && c.state.Status == docker.StateRunning {
		f.exitLocked(c, code, oom)
	}
}

// RestartDaemon kills every running container with SIGTERM, as a daemon
// stopping does, without the controller having asked.
func (f *fakeDocker) RestartDaemon() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.state.Status == docker.StateRunning {
			f.exitLocked(c, 143, false)
		}
	}
}

// Remove is somebody running docker rm -f.
func (f *fakeDocker) Remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[name]; c != nil {
		delete(f.containers, name)
		f.emit(c, "destroy")
	}
}

// Container returns a copy of a container by name.
func (f *fakeDocker) Container(name string) (fakeContainer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[name]
	if !ok {
		return fakeContainer{}, false
	}
	return *c, true
}

// Count is how many containers and volumes exist.
func (f *fakeDocker) Count() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers), len(f.volumes)
}

// AddImage puts an image on the host.
func (f *fakeDocker) AddImage(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[ref] = true
}
