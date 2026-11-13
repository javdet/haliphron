package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/automagicops/haliphron/backend/store"
)

// Run notes: what a run has learned — how the repository is laid out, which
// command builds it, which test is flaky, what is left to do — written down
// so that the runs it starts can read it instead of paying to learn it again.
//
// A run writes only its own notes and reads its own and its ancestors', the
// nearest level winning on a key present at several. Siblings never see each
// other's notes, a parent never sees its children's, and reads are live. The
// caller is the run its token names, never an argument, and an operator's
// token has no notes: it has no lineage, and nothing seeds notes from outside.
//
// A note is text written by a model that may have read a prompt-injected
// repository, and it is the one channel through which an injection reaches a
// run's whole subtree. So it is data and never instructions: the platform
// never puts a note into a prompt, every rendering says who wrote it, keys are
// refused rather than cleaned, and a value is never logged — log lines carry
// the key, the size and the run.
//
// The physical side — daily partitions dropped whole, never a DELETE per
// expired row — is in backend/store/notes.go and migration 0018.

// Note limits. A value is bounded so that one note is a note and not a file;
// the per-run totals bound what a run can make every descendant carry.
const (
	MaxNoteKeyBytes    = 128
	MaxNoteValueBytes  = 16 << 10
	MaxNotesPerRun     = 64
	MaxNoteBytesPerRun = 256 << 10
	// MaxNotesListed bounds one list_notes answer.
	MaxNotesListed = 200
)

// Note retention bounds.
//
// The default equals MaxGiveUpAfter, so a continuation that waited as long as
// schedule_agent allows still finds its parent's notes. The minimum covers a
// run that queues for a while and then runs for the longest timeout there is;
// below it, a run could see its own notes expire under it.
const (
	DefaultNoteRetention = 7 * 24 * time.Hour
	MinNoteRetention     = 2 * 24 * time.Hour
	MaxNoteRetention     = 30 * 24 * time.Hour
)

// notePartitionsAhead is how many days of partitions exist before they are
// needed. Three days of maintenance would have to fail in a row before an
// insert met a missing day, and then it heals itself.
const notePartitionsAhead = 3 * 24 * time.Hour

// NoteKeyPattern is the one allow-list for keys: letters and digits first,
// then path-like punctuation, nothing that needs quoting anywhere. The MCP
// tool schema states the same pattern by referring to this constant.
const NoteKeyPattern = `^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`

var noteKeyPattern = regexp.MustCompile(NoteKeyPattern)

// ErrNotesNeedRun is a note call from a credential that is not a run's own.
var ErrNotesNeedRun = errors.New("app: only a run's own token has notes")

// Reasons a note call is refused, as NoteRefusal carries them.
const (
	NoteInvalid = "invalid_argument"
	NoteQuota   = "quota_exceeded"
	NoteExpired = "expired"
)

// NoteRefusal is a note call refused for a reason the agent can act on.
type NoteRefusal struct {
	Reason string
	Detail string
}

func (e *NoteRefusal) Error() string { return e.Reason + ": " + e.Detail }

// PutNote writes or overwrites one of the calling run's notes, and reports
// whether the stored value changed.
func (s *Service) PutNote(ctx context.Context, caller *store.Run, key, value string) (bool, error) {
	if caller == nil {
		return false, ErrNotesNeedRun
	}
	if err := validateNoteKey(key); err != nil {
		return false, err
	}
	if err := validateNoteValue(value); err != nil {
		return false, err
	}

	limits := store.NoteLimits{
		MaxNotes: MaxNotesPerRun, MaxBytes: MaxNoteBytesPerRun, Retention: s.limits.NoteRetention,
	}
	changed, err := s.store.PutNote(ctx, caller.ID, key, value, limits)
	if errors.Is(err, store.ErrNotePartitionMissing) {
		// The maintenance has not run, or not recently enough. Create what is
		// missing and try once more; the second attempt checks the expiry
		// again, so a run that crossed it meanwhile is refused as expired.
		s.log.Warn("a note partition was missing; creating it", "run", caller.ID, "key", key)
		if _, err := s.store.EnsureNotePartitions(ctx, s.limits.NoteRetention, notePartitionsAhead); err != nil {
			return false, fmt.Errorf("app: create the missing note partition: %w", err)
		}
		changed, err = s.store.PutNote(ctx, caller.ID, key, value, limits)
	}

	var quota *store.NoteQuotaError
	switch {
	case errors.As(err, &quota):
		return false, &NoteRefusal{Reason: NoteQuota, Detail: fmt.Sprintf(
			"this run has %d of %d notes using %d of %d bytes, and this one adds %d; "+
				"delete a note it no longer needs, or overwrite one", quota.Notes, quota.MaxNotes,
			quota.Bytes, quota.MaxBytes, quota.Adding)}
	case errors.Is(err, store.ErrNoteExpired):
		return false, &NoteRefusal{Reason: NoteExpired, Detail: fmt.Sprintf(
			"this run was admitted more than %s ago, and its notes have expired", s.limits.NoteRetention)}
	case errors.Is(err, store.ErrNotePartitionMissing):
		s.log.Error("a note partition is still missing after creating the partitions", "run", caller.ID, "key", key)
		return false, err
	case err != nil:
		return false, err
	}
	s.log.Debug("note written", "run", caller.ID, "key", key, "bytes", len(value), "changed", changed)
	return changed, nil
}

// Note reads one key from the calling run's scope.
func (s *Service) Note(ctx context.Context, caller *store.Run, key string) (store.Note, bool, error) {
	if caller == nil {
		return store.Note{}, false, ErrNotesNeedRun
	}
	if err := validateNoteKey(key); err != nil {
		return store.Note{}, false, err
	}
	notes, _, err := s.store.ReadNotes(ctx, caller.ID,
		store.NoteQuery{Key: key, Values: true, Limit: 1}, s.limits.NoteRetention)
	if err != nil || len(notes) == 0 {
		return store.Note{}, false, err
	}
	return notes[0], true, nil
}

// Notes lists the keys in the calling run's scope, without values, and
// reports whether there were more than MaxNotesListed.
func (s *Service) Notes(ctx context.Context, caller *store.Run, prefix string) ([]store.Note, bool, error) {
	if caller == nil {
		return nil, false, ErrNotesNeedRun
	}
	if prefix != "" && len(prefix) > MaxNoteKeyBytes {
		return nil, false, &NoteRefusal{Reason: NoteInvalid,
			Detail: fmt.Sprintf("prefix: %d bytes is longer than any key", len(prefix))}
	}
	return s.store.ReadNotes(ctx, caller.ID,
		store.NoteQuery{Prefix: prefix, Limit: MaxNotesListed}, s.limits.NoteRetention)
}

// DeleteNote removes one of the calling run's own notes, and reports whether
// there was one. An ancestor's note is never removed, and the answer does not
// say whether one exists under that key.
func (s *Service) DeleteNote(ctx context.Context, caller *store.Run, key string) (bool, error) {
	if caller == nil {
		return false, ErrNotesNeedRun
	}
	if err := validateNoteKey(key); err != nil {
		return false, err
	}
	return s.store.DeleteNote(ctx, caller.ID, key)
}

// validateNoteKey applies the allow-list. A key outside it is refused, never
// cleaned: a key silently altered is a key the reader looks up and does not
// find, or worse, finds someone else's under.
func validateNoteKey(key string) error {
	if !noteKeyPattern.MatchString(key) {
		shown := key
		if len(shown) > MaxNoteKeyBytes {
			shown = shown[:MaxNoteKeyBytes] + "…"
		}
		return &NoteRefusal{Reason: NoteInvalid, Detail: fmt.Sprintf(
			"key: %q is not a note key; a key is 1 to %d characters, starts with a letter or digit, "+
				"and holds only letters, digits and . _ : / -", shown, MaxNoteKeyBytes)}
	}
	return nil
}

// validateNoteValue bounds a value and refuses what would hide text from the
// person reading it later: control characters other than line breaks and
// tabs, and the bidirectional overrides that make text render in an order
// other than the one it is read in. NUL is among the former, and PostgreSQL
// could not store it anyway.
func validateNoteValue(value string) error {
	switch {
	case value == "":
		return &NoteRefusal{Reason: NoteInvalid, Detail: "value: is required; delete_note removes a note"}
	case len(value) > MaxNoteValueBytes:
		return &NoteRefusal{Reason: NoteInvalid, Detail: fmt.Sprintf(
			"value: %d bytes is over the %d-byte limit; a note is a summary, not a file", len(value), MaxNoteValueBytes)}
	case !utf8.ValidString(value):
		return &NoteRefusal{Reason: NoteInvalid, Detail: "value: is not valid UTF-8"}
	}
	for _, r := range value {
		if (unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r') || isBidiControl(r) {
			return &NoteRefusal{Reason: NoteInvalid, Detail: fmt.Sprintf(
				"value: contains the control character %U; write plain text", r)}
		}
	}
	return nil
}

// NoteMaintenance is what one maintenance pass did.
type NoteMaintenance struct {
	Created []string
	Dropped []string
	// Busy are expired partitions whose drop gave up waiting for its lock;
	// the next pass tries them again.
	Busy []string
}

// MaintainNotes creates the note partitions that will be needed and drops
// those whose notes have all expired. Safe on every replica at once: each
// create and each drop takes one advisory lock and rechecks the catalogue.
func (s *Service) MaintainNotes(ctx context.Context) (NoteMaintenance, error) {
	var out NoteMaintenance
	created, err := s.store.EnsureNotePartitions(ctx, s.limits.NoteRetention, notePartitionsAhead)
	out.Created = created
	if err != nil {
		return out, err
	}
	out.Dropped, out.Busy, err = s.store.DropExpiredNotePartitions(ctx, s.limits.NoteRetention)
	return out, err
}

// RunNoteMaintenance runs MaintainNotes every interval until ctx is done.
func (s *Service) RunNoteMaintenance(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maintainNotesOnce(ctx)
		}
	}
}

// PrepareNotes runs one maintenance pass now, before the MCP listener serves,
// so that the first put_note after an install or a long outage does not have
// to create its partition itself.
func (s *Service) PrepareNotes(ctx context.Context) { s.maintainNotesOnce(ctx) }

func (s *Service) maintainNotesOnce(ctx context.Context) {
	m, err := s.MaintainNotes(ctx)
	if len(m.Created) > 0 {
		s.log.Info("note partitions created", "partitions", m.Created)
	}
	if len(m.Dropped) > 0 {
		s.log.Info("expired note partitions dropped", "partitions", m.Dropped)
	}
	if len(m.Busy) > 0 {
		s.log.Warn("expired note partitions were busy and are kept until the next pass", "partitions", m.Busy)
	}
	if err != nil && !ctxDone(ctx) {
		s.log.Error("note maintenance failed", "error", err)
	}
}

// NoteUsage is the notes table's footprint, for the metrics.
func (s *Service) NoteUsage(ctx context.Context) (store.NoteUsage, error) {
	return s.store.NoteUsage(ctx)
}

// NoteRetention is the configured retention.
func (s *Service) NoteRetention() time.Duration { return s.limits.NoteRetention }
