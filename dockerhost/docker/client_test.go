package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusesMapToTheSentinels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v"+APIVersion+"/") {
			t.Errorf("unversioned path %s", r.URL.Path)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/json"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container"}`)
		case strings.HasSuffix(r.URL.Path, "/create"):
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"message":"name in use"}`)
		}
	}))
	defer srv.Close()
	c, err := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContainerInspect(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.ContainerCreate(context.Background(), "x", ContainerSpec{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("got %v", err)
	}
}

func TestAPullThatFailsMidStreamIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Registry-Auth") != "creds" {
			t.Errorf("auth header %q", r.Header.Get("X-Registry-Auth"))
		}
		_, _ = io.WriteString(w, `{"status":"Pulling"}`+"\n"+`{"error":"manifest unknown"}`+"\n")
	}))
	defer srv.Close()
	c, _ := NewClient("tcp://" + strings.TrimPrefix(srv.URL, "http://"))
	err := c.ImagePull(context.Background(), "x/y:1", "creds")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("got %v", err)
	}
}

func TestDemuxJoinsTheFrames(t *testing.T) {
	var buf bytes.Buffer
	for _, s := range []string{"check|a|0\n", "check|b|28\n"} {
		h := make([]byte, 8)
		h[0] = 1
		binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
		buf.Write(h)
		buf.WriteString(s)
	}
	got, err := Demux(&buf)
	if err != nil || got != "check|a|0\ncheck|b|28\n" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestRegistryCredentialsAreFoundByHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw, _ := json.Marshal(map[string]any{"auths": map[string]any{
		"https://index.docker.io/v1/": map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("u:p"))},
		"registry.example.com:5000":   map[string]string{"identitytoken": "tok"},
	}})
	_ = os.WriteFile(path, raw, 0o600)
	a, err := LoadAuths(path)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(s string) map[string]string {
		raw, _ := base64.URLEncoding.DecodeString(s)
		var m map[string]string
		_ = json.Unmarshal(raw, &m)
		return m
	}
	if m := decode(a.For("javdet/haliphron-agent:1")); m["username"] != "u" || m["password"] != "p" {
		t.Fatalf("docker hub: %v", m)
	}
	if m := decode(a.For("registry.example.com:5000/a/b@sha256:00")); m["identitytoken"] != "tok" {
		t.Fatalf("private: %v", m)
	}
	if a.For("ghcr.io/x/y") != "" {
		t.Fatal("credentials for a registry with none")
	}
	if missing, err := LoadAuths(filepath.Join(t.TempDir(), "nope")); err != nil || missing.For("x") != "" {
		t.Fatal("a missing config is no credentials")
	}
}
