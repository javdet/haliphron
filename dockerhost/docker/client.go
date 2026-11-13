package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is Engine over the daemon's HTTP API.
type Client struct {
	http *http.Client
	base string
}

// NewClient connects to a daemon address: unix:///var/run/docker.sock, or
// tcp://host:port for a daemon on the network (without TLS, which is why the
// install script never configures one).
func NewClient(host string) (*Client, error) {
	switch {
	case strings.HasPrefix(host, "unix://"):
		path := strings.TrimPrefix(host, "unix://")
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
		return &Client{http: &http.Client{Transport: transport}, base: "http://docker"}, nil
	case strings.HasPrefix(host, "tcp://"):
		return &Client{http: &http.Client{}, base: "http://" + strings.TrimPrefix(host, "tcp://")}, nil
	default:
		return nil, fmt.Errorf("docker: unsupported daemon address %q", host)
	}
}

// APIError is a failure the daemon explained.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("docker %s: status %d: %s", e.Op, e.Status, e.Message)
}

// Is maps the two statuses callers branch on to the sentinels.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	}
	return false
}

func (c *Client) url(path string, q url.Values) string {
	u := c.base + "/v" + APIVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// do sends one request and decodes a JSON answer into out, if out is not nil.
func (c *Client) do(ctx context.Context, op, method, path string, q url.Values,
	body any, headers map[string]string, out any) error {

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("docker %s: encode: %w", op, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), reader)
	if err != nil {
		return fmt.Errorf("docker %s: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s: %w", op, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 400 {
		return apiError(op, resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker %s: decode: %w", op, err)
	}
	return nil
}

func apiError(op string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var msg struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &msg) != nil || msg.Message == "" {
		msg.Message = strings.TrimSpace(string(raw))
	}
	return &APIError{Op: op, Status: resp.StatusCode, Message: msg.Message}
}

func labelFilter(labels map[string]string) url.Values {
	if len(labels) == 0 {
		return url.Values{}
	}
	var list []string
	for k, v := range labels {
		if v == "" {
			list = append(list, k)
		} else {
			list = append(list, k+"="+v)
		}
	}
	raw, _ := json.Marshal(map[string][]string{"label": list})
	return url.Values{"filters": {string(raw)}}
}

// Info implements Engine.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.do(ctx, "info", http.MethodGet, "/info", nil, nil, nil, &out)
	return out, err
}

// Version implements Engine.
func (c *Client) Version(ctx context.Context) (VersionInfo, error) {
	var out VersionInfo
	err := c.do(ctx, "version", http.MethodGet, "/version", nil, nil, nil, &out)
	return out, err
}

// ImageInspect implements Engine.
func (c *Client) ImageInspect(ctx context.Context, ref string) (Image, error) {
	var out Image
	err := c.do(ctx, "image inspect", http.MethodGet, "/images/"+escapeRef(ref)+"/json", nil, nil, nil, &out)
	return out, err
}

// ImagePull implements Engine. The daemon answers 200 at once and streams
// progress; a failure part-way through arrives as a JSON message with an
// "error" field in the stream, not as a status code, so the stream is read to
// its end.
func (c *Client) ImagePull(ctx context.Context, ref, auth string) error {
	q := url.Values{"fromImage": {ref}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/images/create", q), nil)
	if err != nil {
		return fmt.Errorf("docker image pull: %w", err)
	}
	if auth != "" {
		req.Header.Set("X-Registry-Auth", auth)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker image pull: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return apiError("image pull", resp)
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker image pull: read progress: %w", err)
		}
		if msg.Error != "" {
			return &APIError{Op: "image pull", Status: http.StatusInternalServerError, Message: msg.Error}
		}
	}
}

// ImageRemove implements Engine.
func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	return c.do(ctx, "image remove", http.MethodDelete, "/images/"+escapeRef(ref), nil, nil, nil, nil)
}

// ContainerCreate implements Engine.
func (c *Client) ContainerCreate(ctx context.Context, name string, spec ContainerSpec) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	err := c.do(ctx, "container create", http.MethodPost, "/containers/create", q, spec, nil, &out)
	return out.ID, err
}

// ContainerStart implements Engine. A 304 (already started) is success.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.do(ctx, "container start", http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil, nil)
}

// ContainerKill implements Engine. A 409 means the container is not running,
// which is what the caller wanted anyway.
func (c *Client) ContainerKill(ctx context.Context, id, signal string) error {
	err := c.do(ctx, "container kill", http.MethodPost, "/containers/"+url.PathEscape(id)+"/kill",
		url.Values{"signal": {signal}}, nil, nil, nil)
	if errors.Is(err, ErrConflict) {
		return nil
	}
	return err
}

// ContainerInspect implements Engine.
func (c *Client) ContainerInspect(ctx context.Context, idOrName string) (Container, error) {
	var out Container
	err := c.do(ctx, "container inspect", http.MethodGet, "/containers/"+url.PathEscape(idOrName)+"/json", nil, nil, nil, &out)
	return out, err
}

// ContainerRemove implements Engine. Anonymous volumes go with the container;
// the named volumes of an attempt are removed separately by name, because the
// daemon's v=true does not touch named volumes.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	q := url.Values{"v": {"true"}, "force": {strconv.FormatBool(force)}}
	return c.do(ctx, "container remove", http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil, nil)
}

// ContainerList implements Engine. Stopped containers are included.
func (c *Client) ContainerList(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	q := labelFilter(labels)
	q.Set("all", "true")
	var out []ContainerSummary
	err := c.do(ctx, "container list", http.MethodGet, "/containers/json", q, nil, nil, &out)
	return out, err
}

// ContainerLogs implements Engine: stdout and stderr of a container started
// without a TTY, demultiplexed, capped at 1 MiB. Only the network probe reads
// logs; agent logs are the operator's to read with docker logs.
func (c *Client) ContainerLogs(ctx context.Context, id string) (string, error) {
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("/containers/"+url.PathEscape(id)+"/logs", q), nil)
	if err != nil {
		return "", fmt.Errorf("docker container logs: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("docker container logs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", apiError("container logs", resp)
	}
	return Demux(io.LimitReader(resp.Body, 1<<20))
}

// Demux reads the daemon's multiplexed log stream: frames of an 8-byte header
// (stream, three bytes of padding, a big-endian length) and a payload.
func Demux(r io.Reader) (string, error) {
	var out strings.Builder
	br := bufio.NewReader(r)
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out.String(), nil
			}
			return out.String(), err
		}
		size := binary.BigEndian.Uint32(header[4:])
		if _, err := io.CopyN(&out, br, int64(size)); err != nil {
			return out.String(), nil
		}
	}
}

// VolumeRemove implements Engine.
func (c *Client) VolumeRemove(ctx context.Context, name string, force bool) error {
	return c.do(ctx, "volume remove", http.MethodDelete, "/volumes/"+url.PathEscape(name),
		url.Values{"force": {strconv.FormatBool(force)}}, nil, nil, nil)
}

// VolumeList implements Engine.
func (c *Client) VolumeList(ctx context.Context, labels map[string]string) ([]Volume, error) {
	var out struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.do(ctx, "volume list", http.MethodGet, "/volumes", labelFilter(labels), nil, nil, &out)
	return out.Volumes, err
}

// NetworkInspect implements Engine.
func (c *Client) NetworkInspect(ctx context.Context, name string) (Network, error) {
	var out Network
	err := c.do(ctx, "network inspect", http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, nil, &out)
	return out, err
}

// Events implements Engine.
func (c *Client) Events(ctx context.Context, labels map[string]string) (<-chan Event, <-chan error) {
	events := make(chan Event, 64)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		filters := map[string][]string{"type": {"container"}}
		for k, v := range labels {
			filters["label"] = append(filters["label"], k+"="+v)
		}
		raw, _ := json.Marshal(filters)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.url("/events", url.Values{"filters": {string(raw)}}), nil)
		if err != nil {
			errs <- err
			return
		}
		// The events stream has no deadline of its own: it is open for as long
		// as the controller runs, and the context is what ends it.
		resp, err := (&http.Client{Transport: c.http.Transport}).Do(req)
		if err != nil {
			errs <- fmt.Errorf("docker events: %w", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			errs <- apiError("events", resp)
			return
		}
		dec := json.NewDecoder(resp.Body)
		for {
			var ev Event
			if err := dec.Decode(&ev); err != nil {
				errs <- fmt.Errorf("docker events: %w", err)
				return
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				errs <- ctx.Err()
				return
			}
		}
	}()
	return events, errs
}

// escapeRef keeps the slashes of an image reference: the daemon routes
// /images/{name}/json with the name's slashes in the path.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
