package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/forge"
	"github.com/automagicops/haliphron/backend/run"
)

// Inspecting a marketplace is how the role form fills itself in: given the
// repository an operator typed, it reads the catalogue's manifest and reports
// the name the catalogue gives itself — the `@suffix` every plugin from it is
// installed as — and the plugins it offers.
//
// It is advisory and nothing else. Nothing is stored, and a role is validated
// on save exactly as it was before; the pod clones the marketplace itself and
// the CLI registers it under whatever the manifest says at that moment. So a
// failure here is never an error the operator has to resolve: it is reported
// as Problem, the form fills in nothing, and the name can still be typed.

// RepoFiles reads one file of a hosted repository at a ref.
type RepoFiles interface {
	ReadFile(ctx context.Context, source, ref, path, token string) ([]byte, error)
}

// marketplaceManifests are where a catalogue keeps its manifest, in the order
// they are tried: claude-code's, then codex's.
var marketplaceManifests = []string{
	".claude-plugin/marketplace.json",
	".agents/plugins/marketplace.json",
}

// Bounds on what is reported. A manifest is operator-chosen and read from a
// repository that may not be; these keep a hostile one from filling a form.
const (
	maxInspectedPlugins     = 500
	maxInspectedDescription = 300
)

// MarketplaceInspection is what a catalogue says about itself.
type MarketplaceInspection struct {
	URL string
	Ref string
	// Found is whether a manifest was read and names itself usably. When it is
	// false, Problem says why and the other fields are empty.
	Found        bool
	Name         string
	Description  string
	ManifestPath string
	Plugins      []MarketplacePlugin
	Problem      string
}

// MarketplacePlugin is one entry of a manifest.
type MarketplacePlugin struct {
	Name        string
	Description string
	Version     string
}

// InspectMarketplace reads a marketplace's manifest.
//
// The source and ref are checked by the same allow-lists a role is saved
// against, and refused the same way: a source that could not be saved is not
// worth a request to a forge. Everything after that is reported, not returned.
func (s *Service) InspectMarketplace(ctx context.Context, source, ref string) (MarketplaceInspection, error) {
	src, err := runv1.PluginMarketplaceURL(source)
	if err != nil {
		return MarketplaceInspection{}, &run.InvalidRequestError{Field: "url", Detail: err.Error()}
	}
	r, err := runv1.PluginRef(ref)
	if err != nil {
		return MarketplaceInspection{}, &run.InvalidRequestError{Field: "ref", Detail: err.Error()}
	}
	out := MarketplaceInspection{URL: src, Ref: r}

	provider := forge.Provider(src)
	if provider == runv1.GitProviderNone {
		out.Problem = "only GitHub and GitLab repositories can be read from here; " +
			"enter the marketplace's name and plugins by hand"
		return out, nil
	}

	// The token the pod would clone with, so that what reads here is what a run
	// can fetch. Its absence is fine — a public catalogue needs none — and so is
	// a token that will not resolve: the read is tried without it, and a private
	// repository then reports itself unreadable.
	token := ""
	if s.git != nil {
		if t, err := s.git.MintToken(ctx, src, provider, 0); err == nil {
			token = t
		} else {
			s.log.Warn("inspecting a marketplace without a git token", "url", src, "err", err)
		}
	}

	var body []byte
	for _, path := range marketplaceManifests {
		body, err = s.repoFiles.ReadFile(ctx, src, r, path, token)
		if err == nil {
			out.ManifestPath = path
			break
		}
		if !errors.Is(err, forge.ErrNotFound) {
			break
		}
	}
	if err != nil {
		out.Problem = inspectProblem(err, r, token != "")
		return out, nil
	}

	manifest, problem := parseMarketplaceManifest(body)
	if problem != "" {
		out.Problem = out.ManifestPath + ": " + problem
		out.ManifestPath = ""
		return out, nil
	}
	out.Found = true
	out.Name = manifest.Name
	out.Description = manifest.Description
	out.Plugins = manifest.Plugins
	return out, nil
}

// inspectProblem words a failed read for the form.
func inspectProblem(err error, ref string, withToken bool) string {
	at := "the default branch"
	if ref != "" {
		at = ref
	}
	switch {
	case errors.Is(err, forge.ErrNotFound):
		msg := "no " + strings.Join(marketplaceManifests, " or ") + " on " + at +
			": the repository may not exist or may not be a marketplace"
		if withToken {
			return msg + ", or the stored git token cannot read it"
		}
		return msg + ", or it is private and no git token is stored for it"
	case errors.Is(err, forge.ErrDenied):
		return "the forge refused the request (" + err.Error() + "); check the stored git token"
	case errors.Is(err, forge.ErrUnsupported):
		return err.Error()
	}
	return "the repository could not be read: " + err.Error()
}

// marketplaceManifest is what is reported of a manifest.
type marketplaceManifest struct {
	Name        string
	Description string
	Plugins     []MarketplacePlugin
}

// parseMarketplaceManifest reads the few keys the form uses.
//
// Leniently, field by field: a manifest is a document a CLI reads and this
// does not validate it on the CLI's behalf. A version written as a number or a
// description as an object costs that field and not the whole answer. What is
// strict is the name, because it becomes the `@suffix` the role is saved with,
// and a plugin whose name could not be enabled is left out rather than offered.
func parseMarketplaceManifest(body []byte) (marketplaceManifest, string) {
	var raw struct {
		Name        json.RawMessage `json:"name"`
		Description json.RawMessage `json:"description"`
		Metadata    struct {
			Description json.RawMessage `json:"description"`
		} `json:"metadata"`
		Plugins []struct {
			Name        json.RawMessage `json:"name"`
			Description json.RawMessage `json:"description"`
			Version     json.RawMessage `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return marketplaceManifest{}, fmt.Sprintf("not a JSON object: %v", err)
	}

	name := jsonString(raw.Name)
	if name == "" {
		return marketplaceManifest{}, "the manifest has no name"
	}
	if _, err := runv1.PluginMarketplaceName(name); err != nil {
		return marketplaceManifest{}, fmt.Sprintf("the catalogue calls itself %q, "+
			"which a role cannot name as a marketplace", name)
	}

	out := marketplaceManifest{Name: name, Description: jsonString(raw.Description)}
	if out.Description == "" {
		out.Description = jsonString(raw.Metadata.Description)
	}
	out.Description = clip(out.Description, maxInspectedDescription)

	seen := map[string]bool{}
	for _, p := range raw.Plugins {
		plugin := jsonString(p.Name)
		if plugin == "" || seen[plugin] {
			continue
		}
		if _, _, err := runv1.PluginID(plugin + "@" + name); err != nil {
			continue
		}
		seen[plugin] = true
		out.Plugins = append(out.Plugins, MarketplacePlugin{
			Name:        plugin,
			Description: clip(jsonString(p.Description), maxInspectedDescription),
			Version:     clip(jsonString(p.Version), 64),
		})
		if len(out.Plugins) == maxInspectedPlugins {
			break
		}
	}
	return out, ""
}

// jsonString is a raw value if it is a string, and empty otherwise.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
