package entrypoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// The structured output, and the one place in this image where the interface to
// the model is designed rather than merely implemented.
//
// The agent writes the payload. The entrypoint fills in the envelope. The
// architecture originally required the agent to write the whole file, service
// fields included, and that is a design error in an interface to a model: every
// formal requirement placed on it is another chance to burn a forty-minute paid
// run on a forgotten schemaVersion. The machine does not forget. What is left
// for the model is exactly what it was called for — a meaningful object.
//
// Whether the object is required at all depends on the node. A node that
// declared a schema gets its file validated and a failure is exit 12; a node
// that declared nothing gets data: {} and no complaint. In v1 there are no
// workflows and therefore no consumers of structured output, and failing a
// completed run over a file nobody will read is pure loss.

// maxSummaryBytes is the ceiling on summary in both the envelope and the
// report, from their schemas.
const maxSummaryBytes = 64 << 10

// maxAgentOutputBytes bounds what is read from the agent's output file. The
// agent is untrusted and the file is inside the work tree; without a ceiling, a
// run that writes a gigabyte there takes the entrypoint down with it, and the
// entrypoint is the process that still has to persist the result.
const maxAgentOutputBytes = 8 << 20

// maxArtifacts matches the envelope schema. Beyond it the upload becomes the
// run, and an agent that wants to keep more than this has a bug or a plan.
const maxArtifacts = 256

// AgentPayload is what the agent left at FileOutput, and the result of deciding
// what to do about it.
type AgentPayload struct {
	// Data is the object as written, byte for byte. Never nil: a missing file
	// yields {}, so that a consumer never has to branch on null before it can
	// index — because it will forget to.
	Data json.RawMessage
	// Present distinguishes "the agent wrote {}" from "the agent wrote
	// nothing", which are different facts about the run even when they produce
	// the same envelope.
	Present bool
}

// ReadAgentPayload reads FileOutput and validates it against the node's schema
// when there is one.
//
// schema is the contents of the reserved output.schema.json key from the role
// ConfigMap, or nil when the node declared none. That distinction decides
// everything here: with a schema the file is mandatory, without one its absence
// is normal.
func ReadAgentPayload(path string, schema []byte) (*AgentPayload, error) {
	raw, err := readCapped(path, maxAgentOutputBytes)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if len(schema) == 0 {
			return &AgentPayload{Data: json.RawMessage("{}")}, nil
		}
		return nil, fail(runv1.ExitOutputInvalid, "OutputMissing",
			"the node declared an output schema and the agent wrote no %s", path)
	case err != nil:
		if len(schema) == 0 {
			// Unreadable and not required. Worth a line in the log, not worth
			// failing a run whose output nobody is waiting for.
			return &AgentPayload{Data: json.RawMessage("{}")}, nil
		}
		return nil, failWrap(runv1.ExitOutputInvalid, "OutputUnreadable", err, "reading %s", path)
	}

	trimmed := []byte(strings.TrimSpace(string(raw)))
	if len(trimmed) == 0 {
		if len(schema) == 0 {
			return &AgentPayload{Data: json.RawMessage("{}")}, nil
		}
		return nil, fail(runv1.ExitOutputInvalid, "OutputEmpty",
			"the node declared an output schema and %s is empty", path)
	}

	// A JSON object, not merely valid JSON: data is an object in the envelope's
	// schema, and an agent that wrote a bare array would produce a file that
	// fails validation in the backend rather than here, a day later.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, failWrap(runv1.ExitOutputInvalid, "OutputMalformed", err,
			"%s is not a JSON object", path)
	}
	payload := &AgentPayload{Data: json.RawMessage(trimmed), Present: true}

	if len(schema) == 0 {
		return payload, nil
	}
	if err := validateAgainst(schema, trimmed); err != nil {
		return nil, err
	}
	return payload, nil
}

// validateAgainst checks the payload against the node's declared schema. The
// path to the mismatch goes into the message: "does not match the schema" sends
// the user to read both documents, and the one fact they need is which field.
func validateAgainst(schema, payload []byte) error {
	compiler := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	if err != nil {
		// The node's schema is the backend's to render, so a broken one is a
		// configuration fault rather than the agent's: exit 30, not 12. The
		// agent did nothing wrong and a retry will not help either way, but the
		// class decides who gets the bug report.
		return failWrap(runv1.ExitConfig, "OutputSchemaInvalid", err,
			"the node's %s does not parse", runv1.RoleConfigKeyOutputSchema)
	}
	if err := compiler.AddResource(runv1.RoleConfigKeyOutputSchema, doc); err != nil {
		return failWrap(runv1.ExitConfig, "OutputSchemaInvalid", err,
			"the node's %s is not a usable schema", runv1.RoleConfigKeyOutputSchema)
	}
	compiled, err := compiler.Compile(runv1.RoleConfigKeyOutputSchema)
	if err != nil {
		return failWrap(runv1.ExitConfig, "OutputSchemaInvalid", err,
			"the node's %s does not compile", runv1.RoleConfigKeyOutputSchema)
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(string(payload)))
	if err != nil {
		return failWrap(runv1.ExitOutputInvalid, "OutputMalformed", err, "the agent's output does not parse")
	}
	if err := compiled.Validate(value); err != nil {
		return fail(runv1.ExitOutputInvalid, "OutputSchemaMismatch",
			"the agent's output does not match the node's schema: %s", firstViolation(err))
	}
	return nil
}

// violationPrinter renders the library's error kinds. It is a package-level
// value because the library's LocalizedString panics on a nil printer, and a
// nil printer is exactly what an obvious reading of its signature suggests.
var violationPrinter = message.NewPrinter(language.English)

// firstViolation renders the most specific mismatch the validator found. The
// library's full output is a tree and belongs in the log; the report's message
// has a thousand characters and one job — to say which field is wrong.
func firstViolation(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	deepest := ve
	for len(deepest.Causes) > 0 {
		deepest = deepest.Causes[0]
	}
	what := deepest.ErrorKind.LocalizedString(violationPrinter)
	if len(deepest.InstanceLocation) == 0 {
		return what
	}
	return "/" + strings.Join(deepest.InstanceLocation, "/") + ": " + what
}

// BuildEnvelope assembles runs/{runID}/output.json around the agent's payload.
//
// Nothing here is asked of the model. The status in particular is derived from
// the exit code rather than declared: an agent's self-assessment of whether it
// succeeded would be worth exactly what its self-assessment of its own cost is
// worth.
func BuildEnvelope(c *Config, payload *AgentPayload, exitCode int32, summary string, artifacts []runv1.OutputArtifact, now time.Time) *runv1.OutputEnvelope {
	data := payload.Data
	if len(data) == 0 {
		data = json.RawMessage("{}")
	}
	return &runv1.OutputEnvelope{
		SchemaVersion: runv1.OutputSchemaVersion,
		RunID:         c.RunID,
		Attempt:       c.Attempt,
		Agent:         c.Agent,
		Model:         c.Model,
		Status:        runv1.OutputStatusForExitCode(exitCode),
		ProducedAt:    now.UTC().Format(time.RFC3339),
		Summary:       truncateSummary(summary),
		Data:          data,
		Artifacts:     artifacts,
	}
}

// truncateSummary cuts to the schema's ceiling on a line boundary where it can.
// A summary that ends mid-sentence is a summary; one that ends mid-UTF-8 is a
// parse error somewhere downstream.
func truncateSummary(s string) string {
	if len(s) <= maxSummaryBytes {
		return s
	}
	cut := truncate(s, maxSummaryBytes)
	if idx := strings.LastIndexByte(cut, '\n'); idx > maxSummaryBytes/2 {
		cut = cut[:idx]
	}
	return cut
}

// ResultMarkdown is the human-readable summary, and it is written always —
// including when the agent produced no text at all.
//
// A missing object under a fixed key breaks the read path for the backend and
// the UI, which then have to distinguish "not there yet" from "never will be".
// "The agent said nothing" is information too, and it costs one object to say
// it properly.
func ResultMarkdown(c *Config, agentText string, exitCode int32, failure *Failure, timings []runv1.PhaseTiming) string {
	if text := strings.TrimSpace(agentText); text != "" {
		return agentText
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Run %s, attempt %d\n\n", c.RunID, c.Attempt)
	fmt.Fprintf(&b, "The %s agent produced no textual result.\n\n", c.Agent)
	fmt.Fprintf(&b, "- exit code: %d (%s)\n", exitCode, runv1.FailureClassForExitCode(exitCode))
	fmt.Fprintf(&b, "- model: %s\n", c.Model)
	if failure != nil {
		fmt.Fprintf(&b, "- failed phase: %s\n", failure.Phase)
		fmt.Fprintf(&b, "- reason: %s\n", failure.Reason)
		fmt.Fprintf(&b, "- message: %s\n", failure.Message())
	}
	if len(timings) > 0 {
		b.WriteString("\n## Phase timings\n\n| phase | outcome | ms |\n|---|---|---|\n")
		for _, t := range timings {
			fmt.Fprintf(&b, "| %s | %s | %d |\n", t.Phase, t.Outcome, t.DurationMs)
		}
	}
	return b.String()
}

// CollectArtifacts walks DirArtifacts and returns what should be uploaded,
// relative paths in sorted order.
//
// Symlinks are not followed and their targets are not uploaded. The directory
// is written by the agent, which is untrusted and has a shell: a symlink to
// /haliphron/secrets/git-token is two characters of prompt injection away, and
// following it would upload the token to a bucket with a thirty-day retention.
func CollectArtifacts(dir string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// No artifacts directory is the normal case.
			return fs.SkipAll
		case err != nil:
			return err
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			// A symlink, a socket, a device node. Nothing here is a file the
			// agent meant to keep, and one of them is an exfiltration channel:
			// a link to /haliphron/secrets/git-token is two characters of
			// prompt injection away, and following it would upload the token
			// to a bucket with a thirty-day retention.
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// WalkDir cannot produce these, and the check costs nothing. The value
		// becomes a storage key by concatenation with the POST policy's prefix,
		// and a key that climbs out of its own prefix is the one thing that
		// capability is scoped to prevent.
		if strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
			return nil
		}
		found = append(found, rel)
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, failWrap(runv1.ExitAgentError, "ArtifactsUnreadable", err, "walking %s", dir)
	}
	sort.Strings(found)
	if len(found) > maxArtifacts {
		found = found[:maxArtifacts]
	}
	return found, nil
}

// readCapped reads a file the agent may have written, and refuses one larger
// than the ceiling rather than reading the ceiling and calling it the file.
//
// Two properties matter because the agent is untrusted and has a shell.
// O_NOFOLLOW: /workspace/.haliphron/output.json pointed at
// /haliphron/secrets/presigned.json is a JSON object, and it would be read and
// uploaded. And the ceiling is enforced on the bytes actually read rather than
// on a prior Stat, because a file can be replaced between the two.
func readCapped(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%s is over the %d byte ceiling", path, max)
	}
	return body, nil
}
