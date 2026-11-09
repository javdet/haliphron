// Package forge reads single files out of hosted git repositories, through the
// forge's own HTTP API.
//
// It exists for one question the role form asks before a role is saved: what
// does the marketplace in this repository call itself, and what does it offer?
// The answer lives in one small file, and cloning a repository to read it would
// need a git binary the backend image does not carry (it is distroless) and
// would fetch far more than one file. Both GitHub and GitLab serve a file's raw
// content at a ref, which is all this needs.
//
// An unrecognised host is refused with ErrUnsupported rather than guessed at.
// The caller treats every failure here as "the form fills nothing in", so a
// forge this does not speak costs the operator some typing and nothing else.
package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// MaxFileBytes bounds one file read. A marketplace manifest is a few
// kilobytes; anything larger is not one.
const MaxFileBytes = 1 << 20

// requestTimeout bounds one request, so a forge that accepts the connection
// and never answers holds a form's spinner and nothing else.
const requestTimeout = 10 * time.Second

var (
	// ErrUnsupported is a source on a host this package has no API for.
	ErrUnsupported = errors.New("only GitHub and GitLab repositories can be read")
	// ErrNotFound is a 404: no such repository, ref or file — or, on GitHub, a
	// private repository the token cannot see, which GitHub reports the same way.
	ErrNotFound = errors.New("not found")
	// ErrDenied is a 401 or 403.
	ErrDenied = errors.New("access denied")
)

// Client reads files. The zero value is usable.
type Client struct {
	// HTTP is the transport. Nil means a client with requestTimeout.
	HTTP *http.Client
	// GitHubAPI is github.com's API root, for tests. Empty means
	// https://api.github.com.
	GitHubAPI string
}

// Provider is the forge a marketplace source lives on, by the same reading the
// entrypoint applies when it clones one: `owner/repo` is GitHub's shorthand,
// and an https URL is classified by its host as run.ProviderFor does.
func Provider(source string) runv1.GitProvider {
	if !strings.HasPrefix(source, "https://") {
		return runv1.GitProviderGitHub
	}
	host := strings.ToLower(hostOf(source))
	switch {
	case strings.Contains(host, "github."):
		return runv1.GitProviderGitHub
	case strings.Contains(host, "gitlab."):
		return runv1.GitProviderGitLab
	}
	return runv1.GitProviderNone
}

// ReadFile returns one file of the repository at ref; an empty ref is the
// default branch. source is `owner/repo` or an https URL, already checked by
// runv1.PluginMarketplaceURL. The token is sent only to the host source names,
// or to github.com's API for the shorthand.
func (c *Client) ReadFile(ctx context.Context, source, ref, path, token string) ([]byte, error) {
	target, provider, err := c.fileURL(source, ref, path)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
	}
	if provider == runv1.GitProviderGitHub {
		req.Header.Set("Accept", "application/vnd.github.raw+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	// Authorization rather than GitLab's PRIVATE-TOKEN, on both forges: it is
	// the header net/http drops when a redirect leaves the host, and a custom
	// one it would carry along.
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		// url.Error carries the URL, which carries no secret: the token is a
		// header.
		return nil, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrDenied, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered HTTP %d", req.URL.Host, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the response: %w", err)
	}
	if len(body) > MaxFileBytes {
		return nil, fmt.Errorf("the file is larger than %d bytes", MaxFileBytes)
	}
	return body, nil
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: requestTimeout}
}

// fileURL is the API address of one file.
func (c *Client) fileURL(source, ref, path string) (string, runv1.GitProvider, error) {
	host, project, err := split(source)
	if err != nil {
		return "", "", err
	}
	provider := Provider(source)

	switch provider {
	case runv1.GitProviderGitHub:
		owner, repo, ok := strings.Cut(project, "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
			return "", "", fmt.Errorf("%q does not name a GitHub repository as owner/repo", source)
		}
		api := "https://" + host + "/api/v3"
		if host == "github.com" || host == "www.github.com" {
			api = strings.TrimSuffix(c.GitHubAPI, "/")
			if api == "" {
				api = "https://api.github.com"
			}
		}
		u := api + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
			"/contents/" + escapeSegments(path)
		if ref != "" {
			u += "?ref=" + url.QueryEscape(ref)
		}
		return u, provider, nil

	case runv1.GitProviderGitLab:
		if ref == "" {
			ref = "HEAD"
		}
		u := "https://" + host + "/api/v4/projects/" + url.PathEscape(project) +
			"/repository/files/" + url.PathEscape(path) + "/raw?ref=" + url.QueryEscape(ref)
		return u, provider, nil
	}
	return "", "", ErrUnsupported
}

// split is a source's host and its repository path, without a .git suffix.
func split(source string) (host, project string, err error) {
	if !strings.HasPrefix(source, "https://") {
		return "github.com", strings.TrimSuffix(source, ".git"), nil
	}
	u, err := url.Parse(source)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("%q is not a usable URL", source)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		// Credentials in the URL would go to the forge beside the token, and a
		// query or fragment names no repository.
		return "", "", fmt.Errorf("%q carries more than a repository address", source)
	}
	project = strings.Trim(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if project == "" {
		return "", "", fmt.Errorf("%q names no repository", source)
	}
	return strings.ToLower(u.Host), project, nil
}

func hostOf(source string) string {
	rest := strings.TrimPrefix(source, "https://")
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// escapeSegments escapes each segment of a path and keeps its separators,
// which is how GitHub's contents API wants a nested file named.
func escapeSegments(path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
