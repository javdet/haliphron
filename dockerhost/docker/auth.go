package docker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Registry credentials come from the host's Docker config, mounted read-only
// into the controller (the spec's imagePullSecrets name Kubernetes Secrets,
// which do not exist here). Only inline "auths" entries are understood:
// credential helpers are programs on the host the controller's container cannot
// run, and the README says so.

// Auths is a parsed config.json.
type Auths struct {
	entries map[string]authEntry
}

type authEntry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// LoadAuths reads a Docker config file. A missing file is no credentials, not
// an error: most installations pull public images or images already on the
// host.
func LoadAuths(path string) (*Auths, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Auths{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("docker: read %s: %w", path, err)
	}
	var cfg struct {
		Auths map[string]authEntry `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("docker: parse %s: %w", path, err)
	}
	return &Auths{entries: cfg.Auths}, nil
}

// For returns the X-Registry-Auth header for an image reference, or empty.
func (a *Auths) For(ref string) string {
	if a == nil || len(a.entries) == 0 {
		return ""
	}
	registry := RegistryOf(ref)
	for key, e := range a.entries {
		if normalizeRegistry(key) != registry {
			continue
		}
		user, pass := e.Username, e.Password
		if e.Auth != "" {
			if dec, err := base64.StdEncoding.DecodeString(e.Auth); err == nil {
				if u, p, ok := strings.Cut(string(dec), ":"); ok {
					user, pass = u, p
				}
			}
		}
		body := map[string]string{"serveraddress": key}
		if e.IdentityToken != "" {
			body["identitytoken"] = e.IdentityToken
		} else {
			body["username"], body["password"] = user, pass
		}
		raw, _ := json.Marshal(body)
		return base64.URLEncoding.EncodeToString(raw)
	}
	return ""
}

// RegistryOf is the registry host of an image reference, with Docker Hub's
// aliases folded into one name.
func RegistryOf(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io"
	}
	return normalizeRegistry(first)
}

func normalizeRegistry(host string) string {
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return host
}
