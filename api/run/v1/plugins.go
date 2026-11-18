package v1

import (
	"fmt"
	"regexp"
	"strings"
)

// The role's plugins, as they travel and as they are validated.
//
// A plugin is a marketplace entry that every supported runtime can use:
// skills, agents, hooks, MCP servers and executables on the agent's PATH. The
// document below is rendered by the backend into RoleConfigKeyPlugins, mounted
// read-only at MountRoleConfig, and read by the entrypoint's plugins phase.
//
// It is deliberately not a rendered settings.<role>.json. claude-code keeps
// this state as JSON under CLAUDE_CONFIG_DIR, codex as TOML under CODEX_HOME
// and opencode as its own opencode.json; a document shaped like any one of them
// would have to be translated for the others, and the translation would live in
// the backend, which is the one place that must not know what a runtime's
// configuration looks like.
//
// opencode has no marketplaces of its own. A `plugin@marketplace` entry reaches
// it through the entrypoint, which reads the catalogue and copies the plugin's
// skills, agents, commands and MCP servers into opencode's own places; what
// opencode does install natively — npm and git packages — is the separate
// OpenCode list.

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

	// OpenCode are opencode's own plugins: npm packages (`name`,
	// `@scope/name`, either with `@version`) or git packages
	// (`github:owner/repo#ref`, `git+https://host/path#ref`), installed with
	// `opencode plugin add`. Only opencode runs read it — a claude-code or
	// codex run ignores it, as an opencode run ignores nothing else here.
	//
	// It is not a resolution source of its own. The chain picks one source
	// and this list travels in the role's document, so it is installed when
	// the role's document is the source that wins.
	OpenCode []string `json:"opencode,omitempty"`

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
	return p == nil || (len(p.Marketplaces) == 0 && len(p.Enabled) == 0 && len(p.OpenCode) == 0)
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

var (
	// An npm package name, scoped or not, as npm itself admits one: lower case,
	// no leading dot or underscore.
	openCodeNPMPattern = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	// An npm version or range. Spaces are left out: a range that needs one is
	// written with a hyphen-less comparator instead, and a space would be an
	// argument boundary to every shell that ever logs the spec.
	openCodeVersionPattern = regexp.MustCompile(`^[A-Za-z0-9.^~<>=*+|-]+$`)
	// The git hosts' path half: owner/repo, or a deeper path for a forge that
	// nests groups.
	openCodeGitPathPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)+$`)
)

// MaxOpenCodePlugins bounds the native list. Each entry is an npm install
// standing between a paid-for lease and the agent starting.
const MaxOpenCodePlugins = 32

// OpenCodePlugin checks one entry of PluginSpec.OpenCode and returns it
// trimmed.
//
// Registry packages and git packages only. Not a local path — the only local
// path that means anything in a pod is one the entrypoint made — and not a
// tarball URL, which would be code fetched from anywhere with no name to
// review. The spec reaches `opencode plugin add` as one argument, which is why
// a leading dash is refused whatever else it is.
func OpenCodePlugin(raw string) (string, error) {
	spec := strings.TrimSpace(raw)
	switch {
	case spec == "":
		return "", fmt.Errorf("the empty string names no opencode plugin")
	case len(spec) > 512:
		return "", fmt.Errorf("%q is longer than 512 characters", spec)
	case strings.ContainsAny(spec, " \t\n\r"):
		return "", fmt.Errorf("%q contains whitespace", spec)
	case strings.HasPrefix(spec, "-"):
		return "", fmt.Errorf("%q starts with a dash", spec)
	}

	gitRef := func(rest string) error {
		path, ref, hasRef := strings.Cut(rest, "#")
		if !openCodeGitPathPattern.MatchString(path) || strings.Contains(path, "..") {
			return fmt.Errorf("%q names no usable repository", spec)
		}
		if hasRef {
			if _, err := PluginRef(ref); err != nil || ref == "" {
				return fmt.Errorf("%q has an unusable ref", spec)
			}
		}
		return nil
	}
	switch {
	case strings.HasPrefix(spec, "github:"):
		if err := gitRef(strings.TrimPrefix(spec, "github:")); err != nil {
			return "", err
		}
		return spec, nil
	case strings.HasPrefix(spec, "git+https://"):
		rest := strings.TrimPrefix(spec, "git+https://")
		host, path, _ := strings.Cut(rest, "/")
		if !pluginHostPattern.MatchString(host) {
			return "", fmt.Errorf("%q names no usable host", spec)
		}
		if err := gitRef(path); err != nil {
			return "", err
		}
		return spec, nil
	case strings.Contains(spec, ":"):
		return "", fmt.Errorf("%q is neither an npm package nor a github: or git+https: package", spec)
	}

	// name, @scope/name, name@version, @scope/name@version: the version is
	// whatever follows the last @ that is not the scope's.
	name, version := spec, ""
	if i := strings.LastIndex(spec, "@"); i > 0 {
		name, version = spec[:i], spec[i+1:]
		if version == "" {
			return "", fmt.Errorf("%q ends with @ and no version", spec)
		}
	}
	if !openCodeNPMPattern.MatchString(name) || len(name) > 214 {
		return "", fmt.Errorf("%q is not a usable npm package name", name)
	}
	if version != "" && !openCodeVersionPattern.MatchString(version) {
		return "", fmt.Errorf("%q is not a usable npm version", version)
	}
	return spec, nil
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

	if len(spec.OpenCode) > MaxOpenCodePlugins {
		return "plugins.opencode", fmt.Errorf("a role may name at most %d opencode plugins",
			MaxOpenCodePlugins)
	}
	native := map[string]bool{}
	for i, raw := range spec.OpenCode {
		at := fmt.Sprintf("plugins.opencode[%d]", i)
		plugin, err := OpenCodePlugin(raw)
		if err != nil {
			return at, err
		}
		if native[plugin] {
			return at, fmt.Errorf("%q is named twice", plugin)
		}
		native[plugin] = true
		spec.OpenCode[i] = plugin
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
