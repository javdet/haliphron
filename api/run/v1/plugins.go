package v1

import (
	"fmt"
	"regexp"
	"strings"
)

// The role's plugins, as they travel and as they are validated.
//
// A plugin is a marketplace entry that both supported runtimes can install:
// skills, agents, hooks, MCP servers and executables on the agent's PATH. The
// document below is rendered by the backend into RoleConfigKeyPlugins, mounted
// read-only at MountRoleConfig, and read by the entrypoint's plugins phase.
//
// It is deliberately not a rendered settings.<role>.json. claude-code keeps
// this state as JSON under CLAUDE_CONFIG_DIR and codex as TOML under
// CODEX_HOME; a document shaped like either one would have to be translated for
// the other, and the translation would live in the backend, which is the one
// place that must not know what a runtime's configuration looks like.

// PluginSpec is a role's plugin configuration.
type PluginSpec struct {
	// Marketplaces are catalogues to register before installing. Each is
	// fetched by the entrypoint with the run's own git credential and then
	// handed to the runtime as a local path — see the plugins phase for why
	// the CLI is never asked to do the clone itself.
	Marketplaces []PluginMarketplace `json:"marketplaces,omitempty"`

	// Enabled are the plugins to install, as `plugin@marketplace`. The
	// marketplace half is the name the catalogue declares for itself, which is
	// not derivable from the repository that carries it: `playneta/claude-plugin`
	// may well declare itself `playneta`.
	Enabled []string `json:"enabled,omitempty"`

	// TrustRepositorySources decides whether the cloned repository's own
	// settings may choose marketplaces and plugins.
	//
	// It defaults to true, which is what makes the repository the first source
	// in the resolution chain. Setting it false is the switch for an
	// installation that does not want a repository — untrusted input, by this
	// system's own reckoning — selecting code that then runs beside the agent
	// with its privileges. Nil means unset, so that "the role never said" and
	// "the role said false" stay distinguishable across a round trip.
	TrustRepositorySources *bool `json:"trustRepositorySources,omitempty"`
}

// PluginMarketplace is one catalogue.
type PluginMarketplace struct {
	// Name is optional and informational: the catalogue names itself, and the
	// entrypoint reports what it actually registered. It is carried so the UI
	// can offer the `@suffix` completions without a round trip to a clone.
	Name string `json:"name,omitempty"`
	// URL is `owner/repo` or an https git URL.
	URL string `json:"url"`
	// Ref is a branch or tag. Empty means the default branch.
	Ref string `json:"ref,omitempty"`
}

// IsEmpty reports whether the spec asks for nothing. An empty spec is how a
// source declines to answer, which is what makes the resolution chain a chain
// rather than a lookup: the first source with something to say wins.
func (p *PluginSpec) IsEmpty() bool {
	return p == nil || (len(p.Marketplaces) == 0 && len(p.Enabled) == 0)
}

// Declares reports whether the role said anything about plugins at all,
// including only that the repository may not choose them.
//
// Distinct from IsEmpty, and the difference is load-bearing. IsEmpty answers
// the resolution chain's question — "has this source any plugins to offer" —
// and a role that only forbids the repository has none. Declares answers the
// backend's question, "is there a document to send", and there is: without it
// the pod finds no file, applies the default, and the one switch that turns the
// repository off silently turns it back on.
func (p *PluginSpec) Declares() bool {
	if p == nil {
		return false
	}
	return !p.IsEmpty() || p.TrustRepositorySources != nil
}

// TrustsRepository applies the default.
func (p *PluginSpec) TrustsRepository() bool {
	if p == nil || p.TrustRepositorySources == nil {
		return true
	}
	return *p.TrustRepositorySources
}

// The single allow-lists. Both the backend's role validation and the
// entrypoint's plugins phase call these, by the same rule that puts
// ArtifactKey in one place: a name one side accepts and the other refuses is a
// role that saves cleanly and fails an hour later, in a pod, with the run paid
// for.

var (
	// A marketplace or plugin name, as both CLIs spell one.
	pluginNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// owner/repo, the shorthand both CLIs accept.
	pluginRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// A git ref, without the ways one can be made to mean something else.
	pluginRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	// A host, optionally with a port. It must start and end with a letter or
	// digit, which is what stops "..", "." and a bare dotted run from passing
	// for one — and those, further down, become a directory name.
	pluginHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
)

// PluginMarketplaceURL checks a marketplace source and returns it trimmed.
//
// Only `owner/repo` and https are admitted. Not ssh, because the pod holds an
// https token and no key; not a local path, because the only local path that
// means anything here is one the entrypoint made itself; and not `git://` or a
// bare host, because neither survives the credential helper.
func PluginMarketplaceURL(raw string) (string, error) {
	url := strings.TrimSpace(raw)
	switch {
	case url == "":
		return "", fmt.Errorf("a marketplace needs a url")
	case len(url) > 2048:
		return "", fmt.Errorf("the marketplace url is longer than 2048 characters")
	case strings.ContainsAny(url, " \t\n\r"):
		return "", fmt.Errorf("%q contains whitespace", url)
	case strings.HasPrefix(url, "-"):
		// A source that starts with a dash is read as a flag by whichever
		// command is handed it, whatever quoting is used.
		return "", fmt.Errorf("%q starts with a dash", url)
	case pluginRepoPattern.MatchString(url):
		return url, nil
	case strings.HasPrefix(url, "https://"):
		rest := strings.TrimPrefix(url, "https://")
		host, _, _ := strings.Cut(rest, "/")
		if !pluginHostPattern.MatchString(host) {
			return "", fmt.Errorf("%q names no usable host", url)
		}
		return url, nil
	}
	return "", fmt.Errorf("%q is neither owner/repo nor an https:// url", url)
}

// PluginRef checks a git ref.
func PluginRef(raw string) (string, error) {
	ref := strings.TrimSpace(raw)
	switch {
	case ref == "":
		return "", nil
	case len(ref) > 255:
		return "", fmt.Errorf("the ref is longer than 255 characters")
	case strings.Contains(ref, ".."):
		return "", fmt.Errorf("%q is not a usable ref", ref)
	case !pluginRefPattern.MatchString(ref):
		return "", fmt.Errorf("%q is not a usable ref", ref)
	}
	return ref, nil
}

// PluginID checks a `plugin@marketplace` identifier and returns its halves.
//
// The marketplace half is required. Both CLIs accept a bare plugin name and
// resolve it against every catalogue they know, which is exactly the ambiguity
// a role should not be able to express: two marketplaces offering the same name
// would install whichever the CLI happened to prefer that day.
func PluginID(raw string) (plugin, marketplace string, err error) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", "", fmt.Errorf("the empty string names no plugin")
	}
	if len(id) > 512 {
		return "", "", fmt.Errorf("%q is longer than 512 characters", id)
	}
	plugin, marketplace, found := strings.Cut(id, "@")
	if !found {
		return "", "", fmt.Errorf("%q does not name a marketplace; "+
			"plugins are installed as plugin@marketplace", id)
	}
	if !pluginNamePattern.MatchString(plugin) {
		return "", "", fmt.Errorf("%q is not a usable plugin name", plugin)
	}
	if !pluginNamePattern.MatchString(marketplace) {
		return "", "", fmt.Errorf("%q is not a usable marketplace name", marketplace)
	}
	return plugin, marketplace, nil
}

// PluginMarketplaceName checks the optional declared name.
func PluginMarketplaceName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", nil
	}
	if !pluginNamePattern.MatchString(name) {
		return "", fmt.Errorf("%q is not a usable marketplace name", name)
	}
	return name, nil
}

// Limits on the document as a whole. A role is operator-authored and
// admin-scoped, so these are guards against a runaway rather than a policy:
// each entry is a clone or an install standing between a paid-for lease and the
// agent starting.
const (
	MaxPluginMarketplaces = 16
	MaxPluginsEnabled     = 64
)

// ValidatePluginSpec checks a whole document, normalising as it goes. The
// returned field path is suitable for the `field` of an API error.
func ValidatePluginSpec(spec *PluginSpec) (field string, err error) {
	if spec == nil {
		return "", nil
	}
	if len(spec.Marketplaces) > MaxPluginMarketplaces {
		return "plugins.marketplaces", fmt.Errorf("a role may name at most %d marketplaces",
			MaxPluginMarketplaces)
	}
	if len(spec.Enabled) > MaxPluginsEnabled {
		return "plugins.enabled", fmt.Errorf("a role may enable at most %d plugins",
			MaxPluginsEnabled)
	}

	declared := map[string]bool{}
	for i := range spec.Marketplaces {
		m := &spec.Marketplaces[i]
		at := fmt.Sprintf("plugins.marketplaces[%d]", i)

		name, err := PluginMarketplaceName(m.Name)
		if err != nil {
			return at + ".name", err
		}
		url, err := PluginMarketplaceURL(m.URL)
		if err != nil {
			return at + ".url", err
		}
		ref, err := PluginRef(m.Ref)
		if err != nil {
			return at + ".ref", err
		}
		m.Name, m.URL, m.Ref = name, url, ref
		if name != "" {
			if declared[name] {
				return at + ".name", fmt.Errorf("%q is named twice", name)
			}
			declared[name] = true
		}
	}

	seen := map[string]bool{}
	for i, raw := range spec.Enabled {
		at := fmt.Sprintf("plugins.enabled[%d]", i)
		plugin, marketplace, err := PluginID(raw)
		if err != nil {
			return at, err
		}
		id := plugin + "@" + marketplace
		if seen[id] {
			return at, fmt.Errorf("%q is enabled twice", id)
		}
		seen[id] = true
		spec.Enabled[i] = id

		// Unconditional, because the resolution chain does not merge sources.
		// Whichever source wins is the only one that speaks, so a document that
		// enables a plugin without saying where it comes from describes an
		// install that cannot succeed: nothing would ever register the
		// catalogue. Refusing here turns that into a message on the role form
		// instead of an exit 30 an hour later, in a pod, with the lease cut.
		if !declared[marketplace] {
			return at, fmt.Errorf("no marketplace named %q is declared alongside it; "+
				"a plugin is installed from a marketplace this same document names", marketplace)
		}
	}
	return "", nil
}
