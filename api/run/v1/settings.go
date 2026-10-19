package v1

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// A claude-code settings.json, read for its plugin half.
//
// Two sides read this file and they must read it alike. The entrypoint reads a
// repository's .claude/settings*.json; the backend reads a role's own
// settings.json when the role declares its plugins that way rather than under
// `plugins`. One parser, for the reason ArtifactKey is one allow-list: a
// marketplace the backend accepts at save and the pod drops at lease time is a
// plugin the operator believes is installed and the agent never had.
//
// Only enabledPlugins and extraKnownMarketplaces are read. The rest of the file
// is claude-code's business — and deliberately not applied, because its
// permissions block would widen the tool ceiling the backend already computed.

// ClaudeSettings is the part of a settings.json that names plugins. The shape
// is claude-code's own. It never travels, so it has no deep copy.
//
// +kubebuilder:object:generate=false
type ClaudeSettings struct {
	ExtraKnownMarketplaces map[string]ClaudeMarketplace `json:"extraKnownMarketplaces,omitempty"`
	EnabledPlugins         map[string]bool              `json:"enabledPlugins,omitempty"`
}

// ClaudeMarketplace is one entry of extraKnownMarketplaces.
//
// +kubebuilder:object:generate=false
type ClaudeMarketplace struct {
	Source struct {
		Source string `json:"source"`
		Repo   string `json:"repo,omitempty"`
		URL    string `json:"url,omitempty"`
		Path   string `json:"path,omitempty"`
		Ref    string `json:"ref,omitempty"`
	} `json:"source"`
}

// RoleSettingsFiles are the names a role's own settings.json may have among its
// config files, most specific first. settings.<role>.json is the repository
// convention carried over; settings.json is what an operator writing one file
// for one role naturally calls it.
func RoleSettingsFiles(role string) []string {
	if role == "" {
		return []string{"settings.json"}
	}
	return []string{"settings." + role + ".json", "settings.json"}
}

// PluginSpecFromSettings turns a settings.json into a PluginSpec.
//
// strict decides what happens to an entry this system cannot act on: a
// marketplace kind the pod has no way to fetch ("directory", "url"), a name or
// an identifier that does not validate. A repository's file is read leniently —
// it was written for a developer's machine, and a run that was going to succeed
// is not thrown away over it — so such entries are dropped and reported in
// dropped. An operator's file is read strictly, because the operator is at the
// keyboard when it is saved and is the one person who can fix it.
//
// The result is sorted, so two attempts of one run install in one order, and
// validated by ValidatePluginSpec, which is what enforces the ceilings on how
// much a run will clone and install.
func PluginSpecFromSettings(body []byte, strict bool) (spec *PluginSpec, dropped []string, err error) {
	var parsed ClaudeSettings
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("not a settings document: %w", err)
	}

	spec = &PluginSpec{}
	refuse := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		if strict {
			return fmt.Errorf("%s", msg)
		}
		dropped = append(dropped, msg)
		return nil
	}

	names := make([]string, 0, len(parsed.ExtraKnownMarketplaces))
	for name := range parsed.ExtraKnownMarketplaces {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry := parsed.ExtraKnownMarketplaces[name]
		var raw string
		switch entry.Source.Source {
		case "github":
			raw = entry.Source.Repo
		case "git":
			raw = entry.Source.URL
		default:
			// "directory" and "url" name something on the developer's machine
			// or a catalogue this pod cannot authenticate to.
			if err := refuse("extraKnownMarketplaces.%s: source %q cannot be fetched in a pod; "+
				"use \"github\" or \"git\"", name, entry.Source.Source); err != nil {
				return nil, nil, err
			}
			continue
		}
		url, err := PluginMarketplaceURL(raw)
		if err != nil {
			if err := refuse("extraKnownMarketplaces.%s: %s", name, err); err != nil {
				return nil, nil, err
			}
			continue
		}
		clean, err := PluginMarketplaceName(name)
		if err != nil || clean == "" {
			if err == nil {
				err = fmt.Errorf("the name is empty")
			}
			if err := refuse("extraKnownMarketplaces.%s: %s", name, err); err != nil {
				return nil, nil, err
			}
			continue
		}
		ref := ""
		if entry.Source.Ref != "" {
			if ref, err = PluginRef(entry.Source.Ref); err != nil {
				if err := refuse("extraKnownMarketplaces.%s.source.ref: %s", name, err); err != nil {
					return nil, nil, err
				}
				continue
			}
		}
		spec.Marketplaces = append(spec.Marketplaces, PluginMarketplace{Name: clean, URL: url, Ref: ref})
	}

	for id, enabled := range parsed.EnabledPlugins {
		if !enabled {
			continue
		}
		plugin, marketplace, err := PluginID(id)
		if err != nil {
			if err := refuse("enabledPlugins.%s: %s", id, err); err != nil {
				return nil, nil, err
			}
			continue
		}
		spec.Enabled = append(spec.Enabled, plugin+"@"+marketplace)
	}
	slices.Sort(spec.Enabled)
	slices.SortFunc(spec.Marketplaces, func(a, b PluginMarketplace) int {
		return strings.Compare(a.Name+"\x00"+a.URL, b.Name+"\x00"+b.URL)
	})

	if field, err := ValidatePluginSpec(spec); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", field, err)
	}
	return spec, dropped, nil
}
