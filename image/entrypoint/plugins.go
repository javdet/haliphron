package entrypoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The plugins phase: marketplaces registered, plugins installed, before the
// agent starts and after the repository is on disk.
//
// Two decisions shape everything here.
//
// The marketplace is cloned by this process and handed to the CLI as a local
// directory, rather than named to the CLI and cloned by it. Both CLIs would do
// the clone themselves and neither would do it with the run's credential: the
// token lives in a file that only the helper configured in git.go knows how to
// read, and it is kept out of the agent's environment on purpose. Cloning here
// is what makes a private marketplace work at all, and it is also what keeps
// the token on this side of the line — see run.go for why the agent's
// environment is built rather than inherited.
//
// The checkout goes under DirRunPrivate and not into the workspace. A
// marketplace in the work tree is a marketplace in the diff, the commit and the
// pull request.

// pluginSources is the resolution chain's answer: where the plugins came from
// and what they are.
type pluginSources struct {
	spec   *runv1.PluginSpec
	origin string
}

// phasePlugins installs the role's plugins.
func phasePlugins(ctx context.Context, r *Run) error {
	rt, err := runtimeFor(r.cfg.Agent)
	if err != nil {
		return err
	}
	installer, ok := rt.(PluginInstaller)
	if !ok {
		return skip("this runtime does not install plugins")
	}

	found, err := r.resolvePlugins()
	if err != nil {
		return err
	}
	if found.spec.IsEmpty() {
		return skip("no marketplace or plugin is declared by the role or the repository")
	}
	r.logf("plugins: %d marketplace(s), %d plugin(s), from %s",
		len(found.spec.Marketplaces), len(found.spec.Enabled), found.origin)

	for _, m := range found.spec.Marketplaces {
		if err := r.addMarketplace(ctx, installer, m); err != nil {
			return err
		}
	}

	for _, id := range found.spec.Enabled {
		out, err := r.runPluginCommand(ctx, installer.InstallPlugin(r, id))
		if err != nil {
			// Exit 30, not 20: a name that is not in the catalogue will not
			// appear on a second attempt, and the run has not paid for the
			// model yet.
			return fail(runv1.ExitConfig, "PluginInstallFailed",
				"installing %s: %s", id, firstLines(out, 6))
		}
		r.logf("plugins: installed %s", id)
	}
	return nil
}

// addMarketplace fetches a catalogue and registers it with the runtime.
//
// The local checkout is offered first, always. The fallback exists for one
// case: claude-code reserves the names of Anthropic's official marketplaces and
// refuses to let a local directory claim one, so a role naming
// `anthropics/claude-code` would fail on a path that has nothing to do with
// credentials. Those catalogues are public, so handing the CLI the original
// source costs nothing and needs no token.
//
// Only that case falls back. A private marketplace that fails for any other
// reason fails the run, rather than quietly retrying in a way that would ask
// the CLI for a credential it does not have and produce a worse error.
func (r *Run) addMarketplace(ctx context.Context, installer PluginInstaller, m runv1.PluginMarketplace) error {
	dir, err := r.fetchMarketplace(ctx, m)
	if err != nil {
		return err
	}

	out, err := r.runPluginCommand(ctx, installer.AddMarketplace(r, dir))
	if err == nil {
		r.logf("plugins: registered the marketplace from %s", m.URL)
		return nil
	}
	if !reservedMarketplaceName(out) {
		return fail(runv1.ExitConfig, "MarketplaceUnusable",
			"registering the marketplace from %s: %s", m.URL, firstLines(out, 6))
	}

	r.logf("plugins: %s declares a reserved marketplace name; registering it by source", m.URL)
	if out, err := r.runPluginCommand(ctx, installer.AddMarketplace(r, m.URL)); err != nil {
		return fail(runv1.ExitConfig, "MarketplaceUnusable",
			"registering the marketplace from %s: %s", m.URL, firstLines(out, 6))
	}
	r.logf("plugins: registered the marketplace from %s", m.URL)
	return nil
}

// runPluginCommand runs one CLI invocation, logging and redacting its output.
// The output comes back because the one retry this phase makes is decided by
// what the CLI said.
func (r *Run) runPluginCommand(ctx context.Context, cmd Command) (string, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	result, runErr := r.commander.Run(ctx, cmd)
	out := r.redactor.String(buf.String())
	if strings.TrimSpace(out) != "" {
		r.logf("plugins: %s", firstLines(out, 20))
	}
	switch {
	case runErr != nil:
		return out, runErr
	case result.ExitCode != 0:
		return out, fmt.Errorf("%s exited %d", cmd.Path, result.ExitCode)
	}
	return out, nil
}

// reservedMarketplaceName recognises the one refusal worth retrying.
//
// Matching on text is unpleasant and it is what the CLI gives us, the same
// bargain isPermanentGitRefusal makes. Both halves must match, so an unrelated
// message mentioning either word does not trigger the fallback.
func reservedMarketplaceName(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "reserved") && strings.Contains(lower, "marketplace")
}

// resolvePlugins walks the chain, most specific first.
//
// The order is the role chain's own, extended by one: the repository's settings
// for this role, then what the control plane sent, then the repository's
// default settings. The first source that declares anything wins outright —
// sources are not merged, because a repository that adds a marketplace to the
// role's list is a repository choosing code the operator did not.
func (r *Run) resolvePlugins() (pluginSources, error) {
	role, err := r.rolePluginSpec()
	if err != nil {
		return pluginSources{}, err
	}

	// The role decides whether the repository may speak at all. A role that is
	// silent on the question allows it, which is what makes the repository the
	// first source rather than a fallback.
	//
	// The files in question are `.claude/settings*.json`, which is claude-code's
	// own format and says nothing about what codex should load. A codex run
	// therefore takes its plugins from the role and nowhere else, rather than
	// inheriting a list written for the other runtime and issuing `codex plugin
	// add` for every entry of it.
	repoReadable := role.TrustsRepository() && r.cfg.HasRepo() &&
		r.cfg.Agent == runv1.AgentClaudeCode
	repoSource := func(path string) (pluginSources, bool, error) {
		if !repoReadable || path == "" {
			return pluginSources{}, false, nil
		}
		spec, err := r.readSettingsPlugins(path)
		if err != nil {
			return pluginSources{}, false, err
		}
		return pluginSources{spec: spec, origin: path}, !spec.IsEmpty(), nil
	}

	if found, ok, err := repoSource(r.repositoryRoleSettingsPath()); err != nil {
		return pluginSources{}, err
	} else if ok {
		return found, nil
	}
	if !role.IsEmpty() {
		return pluginSources{spec: role, origin: "the role"}, nil
	}
	if found, ok, err := repoSource(r.repositoryDefaultSettingsPath()); err != nil {
		return pluginSources{}, err
	} else if ok {
		return found, nil
	}
	return pluginSources{spec: &runv1.PluginSpec{}}, nil
}

func (r *Run) repositoryRoleSettingsPath() string {
	if r.cfg.Role == "" {
		return ""
	}
	return filepath.Join(r.layout.Workspace, ".claude", "settings."+r.cfg.Role+".json")
}

func (r *Run) repositoryDefaultSettingsPath() string {
	return filepath.Join(r.layout.Workspace, ".claude", "settings.json")
}

// rolePluginSpec reads what the backend rendered into the ConfigMap.
func (r *Run) rolePluginSpec() (*runv1.PluginSpec, error) {
	path := filepath.Join(r.layout.RoleConfig, runv1.RoleConfigKeyPlugins)
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &runv1.PluginSpec{}, nil
	case err != nil:
		return nil, failWrap(runv1.ExitConfig, "RoleConfigUnreadable", err, "reading %s", path)
	}

	var spec runv1.PluginSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return nil, failWrap(runv1.ExitConfig, "RoleConfigUnreadable", err, "parsing %s", path)
	}
	// Validated on the way in as well as on the way out. The backend checks the
	// same document when the role is saved; this is the other half of the rule
	// ArtifactKey states, that a value one side accepts and the other refuses is
	// a failure nobody can see from either end.
	if field, err := runv1.ValidatePluginSpec(&spec); err != nil {
		return nil, fail(runv1.ExitConfig, "RoleConfigInvalid", "%s: %s", field, err)
	}
	return &spec, nil
}

// claudeSettings is the part of a settings.json this phase reads. The shape is
// claude-code's own, because the file is claude-code's own; a repository
// running codex declares its plugins to the role instead.
type claudeSettings struct {
	ExtraKnownMarketplaces map[string]struct {
		Source struct {
			Source string `json:"source"`
			Repo   string `json:"repo"`
			URL    string `json:"url"`
			Path   string `json:"path"`
		} `json:"source"`
	} `json:"extraKnownMarketplaces"`
	EnabledPlugins map[string]bool `json:"enabledPlugins"`
}

// readSettingsPlugins turns a repository's settings file into a PluginSpec.
//
// Everything it returns came out of the cloned repository, so everything it
// returns goes through the same validation the role's own list does — including
// the ceilings on how many catalogues a run will clone and how many plugins it
// will install.
//
// What it cannot use it drops, with a line in the log, rather than failing the
// run. A settings file is written for a developer's machine: it may name a
// marketplace kind this pod has no credential for, or a shape a newer CLI
// understands and this build does not, and neither is a reason to throw away a
// run that was going to succeed.
func (r *Run) readSettingsPlugins(path string) (*runv1.PluginSpec, error) {
	if path == "" {
		return &runv1.PluginSpec{}, nil
	}
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &runv1.PluginSpec{}, nil
	case err != nil:
		return nil, failWrap(runv1.ExitConfig, "RoleConfigUnreadable", err, "reading %s", path)
	}

	var parsed claudeSettings
	if err := json.Unmarshal(body, &parsed); err != nil {
		// A settings file this build cannot parse is not a reason to fail a
		// run: the agent is about to read it for itself, and it may well
		// understand a newer shape than this does.
		return &runv1.PluginSpec{}, nil
	}

	spec := &runv1.PluginSpec{}
	for name, entry := range parsed.ExtraKnownMarketplaces {
		var raw string
		switch entry.Source.Source {
		case "github":
			raw = entry.Source.Repo
		case "git":
			raw = entry.Source.URL
		default:
			// "directory" and "url" name something on the developer's machine
			// or a catalogue this pod cannot authenticate to.
			continue
		}
		url, err := runv1.PluginMarketplaceURL(raw)
		if err != nil {
			continue
		}
		clean, err := runv1.PluginMarketplaceName(name)
		if err != nil || clean == "" {
			continue
		}
		spec.Marketplaces = append(spec.Marketplaces, runv1.PluginMarketplace{Name: clean, URL: url})
	}
	for id, enabled := range parsed.EnabledPlugins {
		if !enabled {
			continue
		}
		plugin, marketplace, err := runv1.PluginID(id)
		if err != nil {
			continue
		}
		spec.Enabled = append(spec.Enabled, plugin+"@"+marketplace)
	}

	// Map iteration is unordered and a run's log should not be. Both lists are
	// sorted so that two attempts of the same run install in the same order.
	sortStrings(spec.Enabled)
	sortMarketplaces(spec.Marketplaces)

	// The same validation the backend applies to a role, applied here to the
	// one source that did not come through it. It is what enforces the ceilings
	// on how many catalogues a run will clone and how many plugins it will
	// install — without it a repository could put a few thousand of either in
	// front of the agent and spend the lease on them.
	//
	// The whole file is dropped rather than repaired. A settings file this
	// build will not act on is a normal thing to find, and half of one is not.
	if field, err := runv1.ValidatePluginSpec(spec); err != nil {
		r.logf("plugins: ignoring %s — %s: %s", path, field, err)
		return &runv1.PluginSpec{}, nil
	}
	return spec, nil
}

// fetchMarketplace clones one catalogue and returns the directory.
func (r *Run) fetchMarketplace(ctx context.Context, m runv1.PluginMarketplace) (string, error) {
	name, err := marketplaceDirName(m)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(r.layout.Marketplaces, name)

	// A resumed attempt may find it already there. Re-cloning would be correct
	// and would also pay for the download twice.
	//
	// The marker is .git rather than .claude-plugin: this process did the
	// clone, so .git is always present, whereas a catalogue is free to keep its
	// manifest in a subdirectory and several do.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		r.logf("plugins: %s is already fetched", m.URL)
		return dir, nil
	}
	if err := os.MkdirAll(r.layout.Marketplaces, 0o700); err != nil {
		return "", failWrap(runv1.ExitConfig, "LayoutUnwritable", err,
			"creating %s", r.layout.Marketplaces)
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", failWrap(runv1.ExitConfig, "LayoutUnwritable", err, "clearing %s", dir)
	}

	args := []string{"clone", "--depth", "1"}
	if m.Ref != "" {
		args = append(args, "--branch", m.Ref)
	}
	args = append(args, marketplaceCloneURL(m.URL), dir)

	out, code, err := r.git(ctx, args...)
	if err != nil {
		return "", gitFailure("MarketplaceUnreachable", out, "cloning the marketplace %s", m.URL)
	}
	if code != 0 {
		return "", gitFailure("MarketplaceUnreachable", out,
			"cloning the marketplace %s (git exited %d)", m.URL, code)
	}
	return dir, nil
}

// marketplaceCloneURL expands the owner/repo shorthand both CLIs accept.
//
// git does not: to git, "owner/repo" is a relative path. The provider is not
// consulted because the shorthand is GitHub's and the CLIs read it as GitHub's;
// a GitLab marketplace is named by its full URL.
func marketplaceCloneURL(url string) string {
	if strings.HasPrefix(url, "https://") {
		return url
	}
	return "https://github.com/" + url
}

// marketplaceDirName is a stable directory name for a source.
//
// It is built rather than taken, because the name it is built from reaches
// os.RemoveAll. "https://.." passes for a URL in more parsers than one would
// like, and a directory called ".." would make the removal delete the whole of
// DirRunPrivate — the credential helper, the prompt and the log with it. So
// every character outside a small set becomes a dash, and a name that is only
// dots is refused rather than cleaned.
func marketplaceDirName(m runv1.PluginMarketplace) (string, error) {
	base := m.Name
	if base == "" {
		base = strings.TrimSuffix(m.URL, ".git")
		base = strings.TrimPrefix(base, "https://")
		base = strings.ReplaceAll(base, "/", "-")
	}

	var b strings.Builder
	for _, c := range base {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	name := b.String()
	if strings.Trim(name, ".") == "" {
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable",
			"%q gives no usable directory name", m.URL)
	}
	return name, nil
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

func sortMarketplaces(in []runv1.PluginMarketplace) {
	key := func(m runv1.PluginMarketplace) string { return m.Name + "\x00" + m.URL }
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && key(in[j]) < key(in[j-1]); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
