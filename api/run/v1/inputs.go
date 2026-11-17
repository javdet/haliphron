package v1

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
)

// Inputs are files that arrive with a request and that the agent is meant to
// read: today, the screenshot or log a person attached to a chat message.
//
// The path they take is fixed by two rules and is the same on both runtimes.
// The backend downloads them, never the pod — the pod holds no chat credential
// and must not be given one — and stores them under the run's prefix at
// admission. The controller fetches them from the backend before it
// acknowledges the lease, so a run it has taken still starts while the backend
// is down. The pod reads them from its controller, over the callback Service it
// already reaches, in RuntimePhaseInputs.

// InputRef is one input file as the lease, the per-run Secret and the pod see
// it.
type InputRef struct {
	// Key is relative to the run's prefix: "inputs/1-screenshot.png". It is
	// built by the backend with InputFileName and checked by InputKey on every
	// side that handles it.
	Key string `json:"key"`
	// Name is what the person called the file. Display only: it is the
	// customer's text, never a path, and never trusted for anything.
	Name string `json:"name"`
	// MediaType is what the bytes sniffed as at admission, never what the chat
	// platform or the file name claimed.
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
	// SHA256 is the lowercase hex digest of the bytes. The controller checks
	// what it fetched against it, and the pod checks what it read.
	SHA256 string `json:"sha256"`
}

// InputManifest is the document under SecretKeyInputs.
type InputManifest struct {
	Inputs []InputRef `json:"inputs"`
}

// File is the input's file name inside DirInputs: its key without the
// prefix. Only meaningful on a key InputKey accepted.
func (r InputRef) File() string { return strings.TrimPrefix(r.Key, StoragePrefixInputs) }

// maxInputFileBytes bounds an input's file name. Long enough for any name a
// person gives a screenshot, short enough that the prompt line naming it stays
// a line.
const maxInputFileBytes = 128

// InputKey checks an input key against the one layout inputs have, and returns
// it unchanged.
//
// It is an allow-list for the reason ArtifactKey states, and the same three
// places apply it: the backend before it stores, the controller before it
// spools, and the pod before it writes into the work tree. One shape —
// inputs/<ordinal>-<name>, the name drawn from a small alphabet that starts
// with a letter or a digit — means no key can be absolute, hidden, a
// traversal, or a spool's own bookkeeping file.
//
// It refuses rather than repairs. InputFileName is where a person's file name
// is made safe, once, by the backend; a key that does not pass here is a
// defect upstream, not something to clean up downstream.
func InputKey(key string) (string, error) {
	file, ok := strings.CutPrefix(key, StoragePrefixInputs)
	switch {
	case key == "":
		return "", errors.New("the empty key names no input")
	case !ok:
		return "", fmt.Errorf("%q is not under %s", key, StoragePrefixInputs)
	case key != path.Clean(key) || strings.Contains(file, "/"):
		return "", fmt.Errorf("%q is not a single file under %s", key, StoragePrefixInputs)
	case len(file) > maxInputFileBytes+4:
		return "", fmt.Errorf("%q is longer than an input file name may be", key)
	case strings.Contains(file, ".."):
		return "", fmt.Errorf("%q contains ..", key)
	case strings.HasSuffix(file, ".meta"), strings.HasSuffix(file, ".partial"):
		return "", fmt.Errorf("%q ends in a reserved suffix", key)
	}

	ordinal, name, ok := strings.Cut(file, "-")
	if !ok || ordinal == "" || len(ordinal) > 3 || name == "" {
		return "", fmt.Errorf("%q is not <ordinal>-<name>", key)
	}
	for _, c := range ordinal {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("%q does not start with an ordinal", key)
		}
	}
	for i, c := range name {
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if i == 0 && !alnum {
			return "", fmt.Errorf("%q: the name must start with a letter or a digit", key)
		}
		if !alnum && c != '.' && c != '_' && c != '-' {
			return "", fmt.Errorf("%q: %q is not allowed in an input file name", key, c)
		}
	}
	return key, nil
}

// InputFileName builds the key of the ordinal-th input from the name a person
// gave the file. The result always passes InputKey.
//
// This is the one place a person's file name is made safe, and it is the
// backend's: every character outside the small alphabet becomes an
// underscore, a leading run of punctuation is dropped, and a long name is cut
// keeping its extension so "the PDF" is still a .pdf. The person's own name is
// kept beside it, in InputRef.Name, for display.
func InputFileName(ordinal int, name string) string {
	ext := path.Ext(name)
	stem := sanitizeInputName(strings.TrimSuffix(name, ext))
	ext = sanitizeInputName(ext)
	if !validInputExt(ext) {
		// An extension that does not survive the alphabet, or one of the
		// spool's reserved suffixes, is part of the stem rather than kept.
		stem = sanitizeInputName(stem + ext)
		ext = ""
	}
	for {
		trimmed := strings.Trim(stem, "._-")
		trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, ".meta"), ".partial")
		if trimmed == stem {
			break
		}
		stem = trimmed
	}
	if stem == "" {
		stem = "file"
	}
	if len(stem)+len(ext) > maxInputFileBytes {
		stem = strings.TrimRight(stem[:maxInputFileBytes-len(ext)], "._-")
	}
	ordinal = max(ordinal, 0) % 1000
	return StoragePrefixInputs + strconv.Itoa(ordinal) + "-" + stem + ext
}

// sanitizeInputName maps a person's text onto the input alphabet: every other
// character becomes one underscore per run, and ".." collapses to ".".
func sanitizeInputName(name string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-'
		if !ok {
			if !lastUnderscore {
				b.WriteByte('_')
			}
			lastUnderscore = true
			continue
		}
		b.WriteRune(c)
		lastUnderscore = false
	}
	safe := b.String()
	for strings.Contains(safe, "..") {
		safe = strings.ReplaceAll(safe, "..", ".")
	}
	return safe
}

// validInputExt is an extension worth keeping: a dot and one to fifteen
// letters or digits, and not a suffix the spool reserves.
func validInputExt(ext string) bool {
	if len(ext) < 2 || len(ext) > 16 || ext[0] != '.' || ext == ".meta" || ext == ".partial" {
		return false
	}
	for _, c := range ext[1:] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
