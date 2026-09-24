package contract_test

import (
	"encoding/json"
	"fmt"
	"testing"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The plugin allow-lists, checked here for the reason ArtifactKey's live in one
// place: the backend refuses a role at PUT time and the entrypoint refuses the
// same document in the pod, and a value one side accepts and the other rejects
// is a role that saves cleanly and fails an hour later with the lease already
// cut and the run already placed.

func TestAMarketplaceSourceIsOwnerRepoOrHTTPS(t *testing.T) {
	t.Parallel()

	good := []string{
		"playneta/claude-plugin",
		"anthropics/claude-code",
		"https://github.com/playneta/claude-plugin",
		"https://gitlab.example.com/group/subgroup/plugins.git",
	}
	for _, url := range good {
		if _, err := runv1.PluginMarketplaceURL(url); err != nil {
			t.Errorf("PluginMarketplaceURL(%q) = %v, want accepted", url, err)
		}
	}

	bad := map[string]string{
		"":                                    "the empty source",
		"   ":                                 "whitespace only",
		"git@github.com:playneta/plugins.git": "ssh, which the pod has no key for",
		"http://github.com/a/b":               "plain http",
		"file:///etc":                         "a local path, which only this process may name",
		"/etc/passwd":                         "an absolute path",
		"../../etc":                           "a traversal",
		"--upload-pack=touch /tmp/x":          "a source that reads as a flag",
		"-oProxyCommand=id":                   "the same, in the short form",
		"https://":                            "no host",
		"playneta/claude plugin":              "a space, which splits the argument",
	}
	for url, why := range bad {
		if got, err := runv1.PluginMarketplaceURL(url); err == nil {
			t.Errorf("PluginMarketplaceURL(%q) = %q, want refused: %s", url, got, why)
		}
	}
}

func TestAPluginIsAlwaysNamedWithItsMarketplace(t *testing.T) {
	t.Parallel()

	plugin, marketplace, err := runv1.PluginID("playneta-infra-coder@playneta")
	if err != nil {
		t.Fatalf("PluginID: %v", err)
	}
	if plugin != "playneta-infra-coder" || marketplace != "playneta" {
		t.Errorf("PluginID split to (%q, %q)", plugin, marketplace)
	}

	// A bare name is the ambiguity a role must not be able to express: both
	// CLIs would resolve it against every catalogue they know, and two
	// marketplaces offering the same name would install whichever the CLI
	// happened to prefer.
	if _, _, err := runv1.PluginID("playneta-infra-coder"); err == nil {
		t.Error("a bare plugin name was accepted; it names no marketplace")
	}
	for _, id := range []string{"", "@playneta", "plugin@", "a@b@c", "../x@y", "plug in@mkt"} {
		if _, _, err := runv1.PluginID(id); err == nil {
			t.Errorf("PluginID(%q) was accepted", id)
		}
	}
}

func TestARoleMayNotEnableAPluginFromAMarketplaceItDoesNotDeclare(t *testing.T) {
	t.Parallel()

	spec := &runv1.PluginSpec{
		Marketplaces: []runv1.PluginMarketplace{{Name: "playneta", URL: "playneta/claude-plugin"}},
		Enabled:      []string{"something@elsewhere"},
	}
	field, err := runv1.ValidatePluginSpec(spec)
	if err == nil {
		t.Fatal("a plugin from an undeclared marketplace was accepted; " +
			"the install would fail in the pod, after the lease was cut")
	}
	if field != "plugins.enabled[0]" {
		t.Errorf("field = %q, want the offending entry so the form can point at it", field)
	}

	// And a document that declares no marketplaces at all is the same case, not
	// a laxer one. The chain picks one source and that source is the only one
	// that speaks, so nothing else is ever going to register the catalogue.
	alone := &runv1.PluginSpec{Enabled: []string{"something@elsewhere"}}
	if _, err := runv1.ValidatePluginSpec(alone); err == nil {
		t.Error("a plugin was enabled with no marketplace named anywhere in the document; " +
			"the install could only fail in the pod")
	}
}

func TestValidationNormalisesSoBothSidesCompareTheSameStrings(t *testing.T) {
	t.Parallel()

	spec := &runv1.PluginSpec{
		Marketplaces: []runv1.PluginMarketplace{{Name: " playneta ", URL: " playneta/claude-plugin ", Ref: " main "}},
		Enabled:      []string{"  playneta-infra-coder@playneta  "},
	}
	if field, err := runv1.ValidatePluginSpec(spec); err != nil {
		t.Fatalf("%s: %v", field, err)
	}
	m := spec.Marketplaces[0]
	if m.Name != "playneta" || m.URL != "playneta/claude-plugin" || m.Ref != "main" {
		t.Errorf("marketplace not normalised: %+v", m)
	}
	if spec.Enabled[0] != "playneta-infra-coder@playneta" {
		t.Errorf("enabled not normalised: %q", spec.Enabled[0])
	}
}

func TestADuplicateIsRefusedRatherThanInstalledTwice(t *testing.T) {
	t.Parallel()

	twice := &runv1.PluginSpec{
		Marketplaces: []runv1.PluginMarketplace{
			{Name: "playneta", URL: "playneta/a"},
			{Name: "playneta", URL: "playneta/b"},
		},
	}
	if _, err := runv1.ValidatePluginSpec(twice); err == nil {
		t.Error("two marketplaces share a name; the second would shadow the first silently")
	}

	same := &runv1.PluginSpec{
		Marketplaces: []runv1.PluginMarketplace{{Name: "m", URL: "org/m"}},
		Enabled:      []string{"a@m", "a@m"},
	}
	if _, err := runv1.ValidatePluginSpec(same); err == nil {
		t.Error("the same plugin was enabled twice")
	}
}

func TestARoleThatSaysNothingAboutTrustAllowsTheRepository(t *testing.T) {
	t.Parallel()

	// The default is the product decision, and it is the one a round trip
	// through JSON must not quietly change: a pointer, so that "unset" and
	// "false" stay distinguishable.
	if !(&runv1.PluginSpec{}).TrustsRepository() {
		t.Error("an unset trustRepositorySources denied the repository")
	}
	no := false
	if (&runv1.PluginSpec{TrustRepositorySources: &no}).TrustsRepository() {
		t.Error("trustRepositorySources=false was not honoured")
	}

	var round runv1.PluginSpec
	encoded, err := json.Marshal(&runv1.PluginSpec{TrustRepositorySources: &no})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.TrustsRepository() {
		t.Errorf("false did not survive the round trip through %s", encoded)
	}
}

func TestAnEmptySpecIsHowASourceDeclinesToAnswer(t *testing.T) {
	t.Parallel()

	// The resolution chain is a chain because of this: the first source with
	// something to say wins, and "nothing to say" has to be expressible.
	var absent *runv1.PluginSpec
	if !absent.IsEmpty() {
		t.Error("a nil spec is not empty")
	}
	if !(&runv1.PluginSpec{}).IsEmpty() {
		t.Error("a zero spec is not empty")
	}
	no := false
	if (&runv1.PluginSpec{TrustRepositorySources: &no}).IsEmpty() != true {
		t.Error("a spec that only forbids the repository should still declare no plugins of its own")
	}
	if (&runv1.PluginSpec{Enabled: []string{"a@m"}}).IsEmpty() {
		t.Error("a spec with a plugin is not empty")
	}
}

func TestAMarketplaceSourceCannotNameADirectoryInsteadOfAHost(t *testing.T) {
	t.Parallel()

	// The source becomes a directory name under the run's private scratch, and
	// that directory is removed before the clone. A source that reduces to ".."
	// would have the removal delete the scratch itself — the credential helper,
	// the prompt and the log with it.
	for _, url := range []string{"https://..", "https://.", "https://../..", "https://-", "https://:8080"} {
		if got, err := runv1.PluginMarketplaceURL(url); err == nil {
			t.Errorf("PluginMarketplaceURL(%q) = %q, want refused: it names no host", url, got)
		}
	}
	// A real host with a port is still a host.
	for _, url := range []string{"https://forge.example.com:8443/org/plugins.git", "https://a.b"} {
		if _, err := runv1.PluginMarketplaceURL(url); err != nil {
			t.Errorf("PluginMarketplaceURL(%q) = %v, want accepted", url, err)
		}
	}
}

func TestARoleThatOnlyForbidsTheRepositoryStillSaysSomething(t *testing.T) {
	t.Parallel()

	no := false
	only := &runv1.PluginSpec{TrustRepositorySources: &no}

	// IsEmpty and Declares answer different questions, and the difference is
	// what keeps the switch working: the chain asks "has this source plugins to
	// offer" (it has not), the backend asks "is there a document to send" (there
	// is). Were the backend to ask IsEmpty, no file would be sent, the pod would
	// find none, and the default would turn the repository back on.
	if !only.IsEmpty() {
		t.Error("a role that only forbids the repository offers no plugins of its own")
	}
	if !only.Declares() {
		t.Error("trustRepositorySources=false would not be sent to the pod, " +
			"so the pod would apply the default and allow the repository")
	}
	if (&runv1.PluginSpec{}).Declares() {
		t.Error("a role that says nothing about plugins would ship an empty document")
	}
	if (&runv1.PluginSpec{Enabled: []string{"a@m"}}).Declares() != true {
		t.Error("a role with plugins declares them")
	}
	var absent *runv1.PluginSpec
	if absent.Declares() {
		t.Error("a nil spec declares nothing")
	}
}

func TestTheCeilingsBoundWhatOneRunWillFetch(t *testing.T) {
	t.Parallel()

	// Applied to the repository's settings as well as to a role, which is the
	// point: a repository that could name a few thousand catalogues could spend
	// the whole lease cloning them.
	many := &runv1.PluginSpec{}
	for i := 0; i <= runv1.MaxPluginMarketplaces; i++ {
		many.Marketplaces = append(many.Marketplaces,
			runv1.PluginMarketplace{Name: fmt.Sprintf("m%d", i), URL: fmt.Sprintf("org/repo%d", i)})
	}
	if field, err := runv1.ValidatePluginSpec(many); err == nil {
		t.Errorf("%d marketplaces were accepted (field %q)", len(many.Marketplaces), field)
	}

	lots := &runv1.PluginSpec{
		Marketplaces: []runv1.PluginMarketplace{{Name: "m", URL: "org/m"}},
	}
	for i := 0; i <= runv1.MaxPluginsEnabled; i++ {
		lots.Enabled = append(lots.Enabled, fmt.Sprintf("p%d@m", i))
	}
	if _, err := runv1.ValidatePluginSpec(lots); err == nil {
		t.Errorf("%d plugins were accepted", len(lots.Enabled))
	}
}
