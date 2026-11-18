package entrypoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Plugins for a runtime with no marketplaces of its own.
//
// opencode installs npm and git packages and nothing else; a skill, an agent
// or a command is a file it finds in a directory. So a role's
// `plugin@marketplace` reaches it by import: the entrypoint clones the
// catalogue as it does for the other runtimes, reads the catalogue's manifest
// itself, and copies each enabled plugin's parts to where opencode looks —
//
//	skills       the plugin's skills directories, added to `skills`
//	agents       agents/<plugin>/<name>.md under the config dir, as subagents
//	commands     commands/<plugin>/<name>.md under the config dir
//	MCP servers  mcp.servers, named plugin_<plugin>_<server>
//
// — and leaves out what opencode cannot run: claude-code's hooks. The server
// name is the one claude-code gives a plugin's server, so a tool policy entry
// written as mcp__plugin_<plugin>_<server>__<tool> means the same tool on both.
//
// The role's own opencode list is installed natively, with `opencode plugin
// add`, before any of that.

// PluginImporter is a runtime whose plugins phase is an import rather than a
// sequence of CLI commands. Asserted for, as PluginInstaller is.
type PluginImporter interface {
	// InstallNative installs one of the runtime's own packages.
	InstallNative(r *Run, spec string) Command
	// ImportPlugin copies one plugin, already on disk, into the runtime's own
	// places, and records what it added.
	ImportPlugin(r *Run, p pluginSource) error
}

// openCodeImports is what the import added that the launch has to name.
type openCodeImports struct {
	// agents are the agent IDs written under the config dir.
	agents []string
	// servers are the MCP servers added to the configuration.
	servers []string
}

// catalogueManifests are where a catalogue keeps its manifest: claude-code's
// place, then codex's. The backend's marketplace inspection reads the same two.
var catalogueManifests = []string{
	".claude-plugin/marketplace.json",
	".agents/plugins/marketplace.json",
}

// pluginManifests are where a plugin keeps its own manifest, which is optional.
var pluginManifests = []string{
	".claude-plugin/plugin.json",
	".codex-plugin/plugin.json",
}

// Bounds on what one plugin may bring. A plugin comes from an operator-chosen
// catalogue, or — with trustRepositorySources — from one a repository named;
// either way it is copied into $HOME, an emptyDir with a size limit, and a
// catalogue that filled it would take the run down with ENOSPC somewhere far
// from here.
const (
	maxPluginFiles = 4000
	maxPluginBytes = 64 << 20
)

// catalogue is a fetched marketplace, read.
type catalogue struct {
	name string
	// base is the directory relative plugin sources resolve against: the
	// checkout, or its metadata.pluginRoot.
	base    string
	entries map[string]catalogueEntry
}

// catalogueEntry is one plugin of a manifest. Components may be declared inline
// in the catalogue rather than in the plugin's own manifest; when they are,
// they are read alongside it.
type catalogueEntry struct {
	Name       string          `json:"name"`
	Source     json.RawMessage `json:"source"`
	Commands   json.RawMessage `json:"commands"`
	Agents     json.RawMessage `json:"agents"`
	Skills     json.RawMessage `json:"skills"`
	MCPServers json.RawMessage `json:"mcpServers"`
	Hooks      json.RawMessage `json:"hooks"`
}

// pluginSource is one enabled plugin, located.
type pluginSource struct {
	plugin      string
	marketplace string
	dir         string
	entry       catalogueEntry
}

func (p pluginSource) id() string { return p.plugin + "@" + p.marketplace }

// importPlugins is the plugins phase for a PluginImporter.
//
// Best effort throughout, by the rule phasePlugins states: a package that will
// not install, a catalogue that cannot be read or a plugin that cannot be
// copied costs the run that plugin, and the agent is told.
func (r *Run) importPlugins(ctx context.Context, importer PluginImporter, spec *runv1.PluginSpec) error {
	for _, native := range spec.OpenCode {
		cmd := importer.InstallNative(r, native)
		if out, err := r.runPluginCommand(ctx, cmd); err != nil {
			r.errorf("plugins: installing the package %s failed, continuing without it: %s", native, firstLines(out, 6))
			r.missingPlugins = append(r.missingPlugins,
				missingPlugin{what: "the plugin " + native, reason: "it did not install"})
			continue
		}
		r.logf("plugins: installed the package %s", native)
	}

	catalogues := map[string]catalogue{}
	unavailable := map[string]string{}
	for _, m := range spec.Marketplaces {
		cat, err := r.loadCatalogue(ctx, m)
		if err != nil {
			reason := pluginProblemReason(err)
			r.errorf("plugins: the marketplace %s is unavailable, continuing without it: %v", m.URL, err)
			r.missingPlugins = append(r.missingPlugins,
				missingPlugin{what: "the marketplace " + marketplaceLabel(m), reason: reason})
			if m.Name != "" {
				unavailable[m.Name] = reason
			}
			continue
		}
		if m.Name != "" && m.Name != cat.name {
			r.logf("plugins: %s calls itself %q, not %q; its plugins are found under %q",
				m.URL, cat.name, m.Name, cat.name)
		}
		catalogues[cat.name] = cat
		r.logf("plugins: read the marketplace %s from %s (%d plugins)", cat.name, m.URL, len(cat.entries))
	}

	for _, id := range spec.Enabled {
		plugin, marketplace, _ := runv1.PluginID(id)
		missing := func(reason string) {
			r.missingPlugins = append(r.missingPlugins, missingPlugin{what: "the plugin " + id, reason: reason})
		}
		cat, ok := catalogues[marketplace]
		if !ok {
			if reason, gone := unavailable[marketplace]; gone {
				r.logf("plugins: not installing %s: its marketplace is unavailable", id)
				missing(reason)
			} else {
				r.errorf("plugins: not installing %s: no marketplace read here calls itself %q", id, marketplace)
				missing("its marketplace was not found")
			}
			continue
		}
		entry, ok := cat.entries[plugin]
		if !ok {
			r.errorf("plugins: not installing %s: the marketplace does not list it", id)
			missing("the marketplace does not list it")
			continue
		}
		dir, err := r.locatePlugin(ctx, cat, entry)
		if err != nil {
			r.errorf("plugins: not installing %s, continuing without it: %v", id, err)
			missing(pluginProblemReason(err))
			continue
		}
		if err := importer.ImportPlugin(r, pluginSource{
			plugin: plugin, marketplace: marketplace, dir: dir, entry: entry,
		}); err != nil {
			r.errorf("plugins: importing %s failed, continuing without it: %v", id, err)
			missing("it could not be set up")
			continue
		}
		r.logf("plugins: installed %s", id)
	}
	return nil
}

// loadCatalogue fetches a marketplace and reads its manifest.
func (r *Run) loadCatalogue(ctx context.Context, m runv1.PluginMarketplace) (catalogue, error) {
	dir, err := r.fetchMarketplace(ctx, m)
	if err != nil {
		return catalogue{}, err
	}
	for _, rel := range catalogueManifests {
		body, err := os.ReadFile(filepath.Join(dir, rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return catalogue{}, failWrap(runv1.ExitConfig, "MarketplaceUnusable", err, "reading %s", rel)
		}
		return parseCatalogue(dir, rel, body)
	}
	return catalogue{}, fail(runv1.ExitConfig, "MarketplaceUnusable",
		"%s has no %s", m.URL, strings.Join(catalogueManifests, " or "))
}

func parseCatalogue(dir, rel string, body []byte) (catalogue, error) {
	var raw struct {
		Name     string `json:"name"`
		Metadata struct {
			PluginRoot string `json:"pluginRoot"`
		} `json:"metadata"`
		Plugins []catalogueEntry `json:"plugins"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return catalogue{}, failWrap(runv1.ExitConfig, "MarketplaceUnusable", err, "parsing %s", rel)
	}
	name, err := runv1.PluginMarketplaceName(raw.Name)
	if err != nil || name == "" {
		return catalogue{}, fail(runv1.ExitConfig, "MarketplaceUnusable",
			"%s names the catalogue %q, which no role can install from", rel, raw.Name)
	}
	base := dir
	if raw.Metadata.PluginRoot != "" {
		if base, err = within(dir, raw.Metadata.PluginRoot); err != nil {
			return catalogue{}, fail(runv1.ExitConfig, "MarketplaceUnusable", "%s: pluginRoot: %v", rel, err)
		}
	}
	cat := catalogue{name: name, base: base, entries: map[string]catalogueEntry{}}
	for _, entry := range raw.Plugins {
		if _, _, err := runv1.PluginID(entry.Name + "@" + name); err != nil {
			continue
		}
		if _, dup := cat.entries[entry.Name]; !dup {
			cat.entries[entry.Name] = entry
		}
	}
	return cat, nil
}

// locatePlugin finds an entry's files: a directory of the catalogue's own
// checkout, or a repository of its own, cloned the way catalogues are.
//
// Every source in a manifest came out of a repository, so each is checked by
// the allow-lists a role's own marketplace is: no path that leaves the
// checkout, no URL a role could not have named.
func (r *Run) locatePlugin(ctx context.Context, cat catalogue, entry catalogueEntry) (string, error) {
	var relative string
	if err := json.Unmarshal(entry.Source, &relative); err == nil {
		return pluginDir(cat.base, relative)
	}
	var source struct {
		Source string `json:"source"`
		Path   string `json:"path"`
		Repo   string `json:"repo"`
		URL    string `json:"url"`
		Ref    string `json:"ref"`
	}
	if err := json.Unmarshal(entry.Source, &source); err != nil {
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable", "the source of %s is neither a path nor an object", entry.Name)
	}
	var url string
	switch source.Source {
	case "local", "path", "relative", "":
		if source.Path == "" {
			return "", fail(runv1.ExitConfig, "MarketplaceUnusable", "the source of %s names no path", entry.Name)
		}
		return pluginDir(cat.base, source.Path)
	case "github":
		url = source.Repo
	case "url", "git":
		url = source.URL
	default:
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable",
			"the source of %s is of kind %q, which this image cannot fetch", entry.Name, source.Source)
	}
	url, err := runv1.PluginMarketplaceURL(url)
	if err != nil {
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable", "the source of %s: %v", entry.Name, err)
	}
	ref, err := runv1.PluginRef(source.Ref)
	if err != nil {
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable", "the source of %s: %v", entry.Name, err)
	}
	return r.fetchMarketplace(ctx, runv1.PluginMarketplace{Name: cat.name + "--" + entry.Name, URL: url, Ref: ref})
}

func pluginDir(base, rel string) (string, error) {
	dir, err := within(base, rel)
	if err != nil {
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable", "%v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fail(runv1.ExitConfig, "MarketplaceUnusable", "%s is not a directory of the marketplace", rel)
	}
	return dir, nil
}

// within resolves rel against base and refuses a result outside base, before
// and after symbolic links are followed. Refused, never cleaned: "../x" cleaned
// is a path inside base, and not the one the manifest meant.
func within(base, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%q is an absolute path", rel)
	}
	joined := filepath.Join(base, rel)
	inside := func(root, path string) bool {
		return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
	}
	if !inside(filepath.Clean(base), joined) {
		return "", fmt.Errorf("%q leaves the directory it is relative to", rel)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(joined); err == nil && !inside(realBase, real) {
		return "", fmt.Errorf("%q leads outside the directory it is relative to", rel)
	}
	return joined, nil
}

// ---------------------------------------------------------------------------
// opencode's half

func (openCode) InstallNative(r *Run, spec string) Command {
	return Command{
		Path: "opencode",
		Args: []string{"plugin", "add", spec},
		// $HOME, not the work tree: the install reads .npmrc from its working
		// directory, and a repository's .npmrc would choose the registry the
		// role's package is fetched from.
		Dir: r.layout.Home,
		Env: r.agentEnv,
	}
}

// pluginComponents are the parts of a plugin, as paths relative to its root.
type pluginComponents struct {
	Commands   json.RawMessage `json:"commands"`
	Agents     json.RawMessage `json:"agents"`
	Skills     json.RawMessage `json:"skills"`
	MCPServers json.RawMessage `json:"mcpServers"`
	Hooks      json.RawMessage `json:"hooks"`
}

func (openCode) ImportPlugin(r *Run, p pluginSource) error {
	dest := filepath.Join(r.openCodePluginRoot(), p.marketplace, p.plugin)
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	// Copied rather than referenced in place: the checkout is under
	// DirRunPrivate, which the agent is not pointed at, and the copy is
	// where ${CLAUDE_PLUGIN_ROOT} is made to point.
	if err := copyPluginTree(p.dir, dest); err != nil {
		return err
	}
	root := filepath.ToSlash(dest)
	rewrite := pluginRewriter(root)

	var parts pluginComponents
	for _, rel := range pluginManifests {
		body, err := os.ReadFile(filepath.Join(dest, rel))
		if err != nil {
			continue
		}
		if err := json.Unmarshal(body, &parts); err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}
		break
	}
	overlay := func(own, inline json.RawMessage) json.RawMessage {
		if len(inline) > 0 {
			return inline
		}
		return own
	}
	parts.Commands = overlay(parts.Commands, p.entry.Commands)
	parts.Agents = overlay(parts.Agents, p.entry.Agents)
	parts.Skills = overlay(parts.Skills, p.entry.Skills)
	parts.MCPServers = overlay(parts.MCPServers, p.entry.MCPServers)
	parts.Hooks = overlay(parts.Hooks, p.entry.Hooks)

	configDir := r.openCodeConfigDir()
	var agents []string
	for _, file := range componentFiles(dest, "agents", parts.Agents, ".md") {
		id, err := writeOpenCodeAgent(file, filepath.Join(configDir, "agents"), p.plugin, rewrite)
		if err != nil {
			return err
		}
		agents = append(agents, id)
	}
	commands := 0
	for _, file := range componentFiles(dest, "commands", parts.Commands, ".md") {
		if err := writeOpenCodeCommand(file, filepath.Join(configDir, "commands"), p.plugin, rewrite); err != nil {
			return err
		}
		commands++
	}
	var skills []string
	for _, dir := range componentPaths(dest, "skills", parts.Skills) {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			skills = append(skills, dir)
		}
	}
	servers, env, err := pluginMCPServers(dest, parts.MCPServers, p.plugin, rewrite)
	if err != nil {
		return err
	}

	hooks := len(parts.Hooks) > 0
	if _, err := os.Stat(filepath.Join(dest, "hooks", "hooks.json")); err == nil {
		hooks = true
	}
	if hooks {
		r.logf("plugins: %s has hooks; opencode does not run claude-code hooks, so they are left out", p.id())
	}

	if err := r.updateOpenCodeConfig(func(doc map[string]any) {
		if len(skills) > 0 {
			existing, _ := doc["skills"].([]any)
			for _, dir := range skills {
				if !slices.Contains(existing, any(dir)) {
					existing = append(existing, dir)
				}
			}
			doc["skills"] = existing
		}
		if len(servers) > 0 {
			into := objectAt(objectAt(doc, "mcp"), "servers")
			for name, server := range servers {
				into[name] = server
			}
		}
	}); err != nil {
		return err
	}

	r.agentEnv = append(r.agentEnv, env...)
	for _, pair := range env {
		if _, value, ok := strings.Cut(pair, "="); ok {
			r.redactor.Add(value)
		}
	}
	for name := range servers {
		r.openCode.servers = append(r.openCode.servers, name)
	}
	slices.Sort(r.openCode.servers)
	r.openCode.agents = append(r.openCode.agents, agents...)
	r.logf("plugins: %s: %d skill dir(s), %d agent(s), %d command(s), %d MCP server(s)",
		p.id(), len(skills), len(agents), commands, len(servers))
	return nil
}

// pluginRewriter turns claude-code's substitutions into what they mean here:
// ${CLAUDE_PLUGIN_ROOT} is the copy, and ${VAR} or ${VAR:-default} is
// opencode's {env:VAR}. A default is dropped; opencode has no syntax for one.
func pluginRewriter(root string) func(string) string {
	return func(s string) string {
		s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", root)
		return pluginVarPattern.ReplaceAllString(s, "{env:$1}")
	}
}

var pluginVarPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-[^}]*)?\}`)

// componentPaths is a component's default directory plus whatever the manifest
// names — claude-code reads both — as paths inside the plugin.
func componentPaths(root, def string, declared json.RawMessage) []string {
	paths := []string{filepath.Join(root, def)}
	var list []string
	var single string
	if err := json.Unmarshal(declared, &single); err == nil && single != "" {
		list = []string{single}
	} else {
		_ = json.Unmarshal(declared, &list)
	}
	for _, rel := range list {
		if path, err := within(root, strings.TrimPrefix(rel, "./")); err == nil && !slices.Contains(paths, path) {
			paths = append(paths, path)
		}
	}
	return paths
}

// componentFiles is every file with the extension under a component's paths,
// each a directory walked or a file named directly.
func componentFiles(root, def string, declared json.RawMessage, ext string) []componentFile {
	var out []componentFile
	for _, path := range componentPaths(root, def, declared) {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			if strings.HasSuffix(path, ext) {
				out = append(out, componentFile{path: path, rel: filepath.Base(path)})
			}
			continue
		}
		_ = filepath.WalkDir(path, func(file string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(file, ext) {
				return nil
			}
			rel, _ := filepath.Rel(path, file)
			out = append(out, componentFile{path: file, rel: rel})
			return nil
		})
	}
	return out
}

type componentFile struct {
	path string
	// rel is the path under the component's directory, which becomes the
	// name: commands/review/deep.md is /<plugin>/review/deep.
	rel string
}

// writeOpenCodeAgent translates a claude-code agent into an opencode subagent
// and returns its ID, <plugin>/<name>.
//
// What carries over is the description, which is how the model chooses the
// agent, and the body, which is its system prompt in both. The tool list and
// the model do not: claude-code's tool names and model aliases mean nothing
// here, and leaving them out gives the agent the run's own model and the run's
// own rules — attached to it by name at launch — rather than a guess at either.
func writeOpenCodeAgent(file componentFile, dir, plugin string, rewrite func(string) string) (string, error) {
	body, err := os.ReadFile(file.path)
	if err != nil {
		return "", err
	}
	meta, text := splitFrontmatter(string(body))
	name := componentName(meta["name"], file.rel)
	id := plugin + "/" + name
	out := "---\n" +
		"description: " + yamlString(orDefault(meta["description"], "The "+name+" agent of the "+plugin+" plugin.")) + "\n" +
		"mode: subagent\n" +
		"---\n" + rewrite(text)
	path := filepath.Join(dir, plugin, name+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return id, os.WriteFile(path, []byte(out), 0o600)
}

// writeOpenCodeCommand translates a claude-code command. $ARGUMENTS and $1… are
// spelled the same in both; the description is kept and the rest of the
// frontmatter — allowed-tools, model — is claude-code's alone.
func writeOpenCodeCommand(file componentFile, dir, plugin string, rewrite func(string) string) error {
	body, err := os.ReadFile(file.path)
	if err != nil {
		return err
	}
	meta, text := splitFrontmatter(string(body))
	rel := strings.TrimSuffix(filepath.ToSlash(file.rel), ".md")
	var parts []string
	for _, part := range strings.Split(rel, "/") {
		parts = append(parts, componentName("", part))
	}
	out := rewrite(text)
	if desc := meta["description"]; desc != "" {
		out = "---\ndescription: " + yamlString(desc) + "\n---\n" + out
	}
	path := filepath.Join(append([]string{dir, plugin}, parts...)...) + ".md"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(out), 0o600)
}

// componentName is a safe file name for a component: its declared name, or its
// file name, with everything outside [A-Za-z0-9._-] turned into a dash.
func componentName(declared, rel string) string {
	name := strings.TrimSpace(declared)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	}
	var b strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		out = "unnamed"
	}
	return out
}

// pluginMCPServers reads a plugin's MCP servers — .mcp.json at its root, and
// whatever its manifest declares, as a path or inline — and renders them.
func pluginMCPServers(root string, declared json.RawMessage, plugin string,
	rewrite func(string) string) (map[string]any, []string, error) {
	var docs [][]byte
	if body, err := os.ReadFile(filepath.Join(root, ".mcp.json")); err == nil {
		docs = append(docs, body)
	}
	var path string
	if err := json.Unmarshal(declared, &path); err == nil && path != "" {
		file, err := within(root, strings.TrimPrefix(path, "./"))
		if err != nil {
			return nil, nil, fmt.Errorf("mcpServers: %w", err)
		}
		if file != filepath.Join(root, ".mcp.json") {
			body, err := os.ReadFile(file)
			if err != nil {
				return nil, nil, fmt.Errorf("mcpServers: %w", err)
			}
			docs = append(docs, body)
		}
	} else if len(bytes.TrimSpace(declared)) > 0 && bytes.TrimSpace(declared)[0] == '{' {
		docs = append(docs, declared)
	}

	servers := map[string]any{}
	var env []string
	for _, body := range docs {
		var wrapped struct {
			Servers map[string]claudeMCPServer `json:"mcpServers"`
		}
		if err := json.Unmarshal(body, &wrapped); err != nil {
			return nil, nil, fmt.Errorf("parsing the MCP servers: %w", err)
		}
		found := wrapped.Servers
		if found == nil {
			// The flat shape: the servers are the document.
			if err := json.Unmarshal(body, &found); err != nil {
				return nil, nil, fmt.Errorf("parsing the MCP servers: %w", err)
			}
		}
		for name, server := range found {
			if server.Command == "" && server.URL == "" {
				continue
			}
			full := "plugin_" + plugin + "_" + name
			rendered, headerEnv := server.openCode(full, rewrite, false)
			servers[full] = rendered
			env = append(env, headerEnv...)
		}
	}
	slices.Sort(env)
	return servers, env, nil
}

// copyPluginTree copies a plugin's files, rewriting claude-code's
// ${CLAUDE_PLUGIN_ROOT} in its markdown — a skill names its own scripts that
// way. Links are not followed and not copied: one pointing at the mounted
// secrets would otherwise become a readable file in $HOME. .git is skipped.
func copyPluginTree(src, dest string) error {
	root := filepath.ToSlash(dest)
	files, total := 0, int64(0)
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dest, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files++
		total += info.Size()
		if files > maxPluginFiles || total > maxPluginBytes {
			return fmt.Errorf("the plugin is larger than %d files or %d MiB", maxPluginFiles, maxPluginBytes>>20)
		}
		mode := os.FileMode(0o600)
		if info.Mode()&0o100 != 0 {
			// A script a skill runs keeps its execute bit.
			mode = 0o700
		}
		if strings.HasSuffix(path, ".md") {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			body = []byte(strings.ReplaceAll(string(body), "${CLAUDE_PLUGIN_ROOT}", root))
			return os.WriteFile(target, body, mode)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

// ---------------------------------------------------------------------------
// Frontmatter

// splitFrontmatter returns the top-level scalar keys of a markdown file's YAML
// header, and the body after it.
//
// A reader for the subset agent and command files use — `key: value`, quoted
// values, and | or > block scalars — rather than a YAML library: two keys are
// read, and this module's dependency graph is the graph of the least trusted
// component in the system. A header it cannot read costs the description,
// never the body.
func splitFrontmatter(text string) (map[string]string, string) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return map[string]string{}, text
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return map[string]string{}, text
	}
	header := rest[:end]
	body := rest[end+len("\n---"):]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}

	meta := map[string]string{}
	lines := strings.Split(header, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		// Indented lines that follow belong to this key.
		var more []string
		for i+1 < len(lines) && (lines[i+1] == "" || lines[i+1][0] == ' ' || lines[i+1][0] == '\t') {
			i++
			more = append(more, strings.TrimSpace(lines[i]))
		}
		switch {
		case strings.HasPrefix(value, "|"):
			value = strings.TrimSpace(strings.Join(more, "\n"))
		case strings.HasPrefix(value, ">"):
			value = strings.Join(strings.Fields(strings.Join(more, " ")), " ")
		case strings.HasPrefix(value, `"`):
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			} else {
				value = strings.Trim(value, `"`)
			}
		case strings.HasPrefix(value, "'"):
			value = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'"), "''", "'")
		default:
			if len(more) > 0 {
				value = strings.Join(strings.Fields(value+" "+strings.Join(more, " ")), " ")
			}
		}
		meta[key] = value
	}
	return meta, body
}

// yamlString quotes a string for a YAML header. A JSON string is a valid YAML
// double-quoted scalar, escapes included.
func yamlString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return `""`
	}
	return strings.TrimSpace(b.String())
}
