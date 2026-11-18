package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
)

// Files attached to a chat message, and to the thread posts its prompt quotes.
//
// The adapter reports what files a post carries and fetches bytes on request;
// which files a run gets, what is said about each one, and the caps are
// decided here. A file that cannot be delivered — too large, past the count,
// a type the agent cannot read, a download that failed — is still named in
// the prompt as not available, so the agent says it cannot see it instead of
// guessing what it showed.

// ChatFile is a file attached to a post, as the adapter decoded it. Every
// field is the platform's claim, and only ID is used to fetch; the type that
// counts is sniffed from the bytes, and the size that counts is the bytes
// read.
type ChatFile struct {
	// ID is the platform's identifier, the handle Download is given.
	ID   string
	Name string
	// MediaType and SizeBytes are what the platform declared. SizeBytes is
	// used only to skip a download that could not pass the cap anyway.
	MediaType string
	SizeBytes int64
	// URL is where the platform serves the bytes, for a platform that names
	// one per file. Fetched only by the adapter, with its own credential.
	URL string
}

// ChatFileError is a file the platform would not hand over, with the reason
// in words the prompt can carry: "the app lacks the files:read scope".
type ChatFileError struct {
	Reason string
}

func (e *ChatFileError) Error() string { return "chat file unavailable: " + e.Reason }

// chatAttachmentLines bounds how many files the block names, delivered or not:
// the delivery cap plus room to say what was left out.
const chatAttachmentExtraLines = 5

// chatAttachmentLineBytes bounds one line of the block. Every value on a line
// is cut to a bound of its own below, and this is their sum with room to
// spare; it is what lets the block's room be held back from the prompt before
// anyone knows which files there are.
const chatAttachmentLineBytes = 640

const (
	chatAttachHead = "--- attached files ---\n" +
		"Files sent with this request. Each one that arrived is in " + runv1.DirInputs + "/ " +
		"under the path shown; open images and PDFs with your file-reading tool, and read " +
		"the others as text. A file marked not available did not reach you: say so " +
		"rather than guess what it contained.\n"
	chatAttachNoRead = "Your role does not allow the file-reading tool, so you may not be " +
		"able to open these files; say so if you cannot.\n"
	chatAttachTail = "--- end of attached files ---\n"
	// chatAttachMoreLine names how many files were not listed at all.
	chatAttachMoreLine = "- %d more files were not listed\n"
	// chatEmptyAskFiles is the request of a message that is a mention and
	// files, and nothing else.
	chatEmptyAskFiles = "(no text beyond the mention: look at the attached files below)"
)

// chatAttachments is what a prompt says about files, and the room it holds
// back for saying it.
type chatAttachments struct {
	// reserve is the room held back from the thread's budget. Fixed before the
	// thread is quoted, from how many files there could be, so that which
	// posts are quoted — and so which posts' files count — is decided once.
	reserve int
	block   string
}

// chatCandidate is a file the run could be given, with where it came from.
type chatCandidate struct {
	file   ChatFile
	poster string
	postID string
}

// chatFileReserve is the room an attachments block can need, given every file
// that could end up in it. Zero when there are none, which leaves a prompt
// without files exactly as it always was.
func chatFileReserve(thread []ChatThreadPost, trigger ChatMessage, limits InputLimits) int {
	n := len(trigger.Files)
	for _, p := range contextPosts(thread, trigger) {
		if chatFilesCount(p) {
			n += len(p.Files)
		}
	}
	if n == 0 {
		return 0
	}
	lines := min(n, limits.MaxFiles+chatAttachmentExtraLines)
	return len(chatAttachHead) + len(chatAttachNoRead) + len(chatAttachTail) +
		(lines+1)*chatAttachmentLineBytes + 2
}

// chatFilesCount reports whether a thread post's files are the person's to
// send: not the bot's own, not other automation's, not the server's.
func chatFilesCount(p ChatThreadPost) bool {
	return len(p.Files) > 0 && !p.FromSelf && !p.FromBot && !p.System
}

// chatCandidates are the files a run could be given, in the order they are
// considered: the triggering message's, then the quoted posts' newest first.
// A post the prompt left out for its byte budget contributes nothing.
func chatCandidates(quoted []ChatThreadPost, trigger ChatMessage) []chatCandidate {
	out := make([]chatCandidate, 0, len(trigger.Files))
	for _, f := range trigger.Files {
		out = append(out, chatCandidate{file: f, poster: trigger.Username, postID: trigger.ID})
	}
	for i := len(quoted) - 1; i >= 0; i-- {
		p := quoted[i]
		if !chatFilesCount(p) {
			continue
		}
		for _, f := range p.Files {
			out = append(out, chatCandidate{file: f, poster: p.Username, postID: p.ID})
		}
	}
	return out
}

// collectFiles downloads what the caps allow and renders the block. It never
// fails: a file that cannot be had is a line saying so.
func (c *Chat) collectFiles(ctx context.Context, candidates []chatCandidate, reserve int) ([]run.InputFile, string) {
	if len(candidates) == 0 {
		return nil, ""
	}
	limits := c.svc.InputLimits()
	maxLines := limits.MaxFiles + chatAttachmentExtraLines

	var (
		files    []run.InputFile
		lines    []string
		total    int64
		failed   int
		unlisted int
		mib      = humanBytes
		unusable = func(cand chatCandidate, reason string) string {
			return fmt.Sprintf("- not available: %s (%s), from %s\n",
				chatFileName(cand.file.Name), reason, chatPoster(cand.poster))
		}
	)
	for _, cand := range candidates {
		if len(lines) >= maxLines {
			unlisted++
			continue
		}
		switch {
		case len(files) >= limits.MaxFiles:
			lines = append(lines, unusable(cand, fmt.Sprintf("over the limit of %d files per request", limits.MaxFiles)))
			continue
		case cand.file.SizeBytes > limits.MaxFileBytes:
			lines = append(lines, unusable(cand, "larger than the "+mib(limits.MaxFileBytes)+" limit"))
			continue
		case cand.file.SizeBytes > 0 && total+cand.file.SizeBytes > limits.MaxBytesPerRun:
			lines = append(lines, unusable(cand, "over the "+mib(limits.MaxBytesPerRun)+" total for one request"))
			continue
		}

		body, err := c.platform.Download(ctx, cand.file, limits.MaxFileBytes)
		if err != nil {
			failed++
			reason := "it could not be downloaded"
			var unavailable *ChatFileError
			if errors.As(err, &unavailable) && unavailable.Reason != "" {
				reason = unavailable.Reason
			}
			lines = append(lines, unusable(cand, reason))
			continue
		}
		size := int64(len(body))
		switch {
		case size > limits.MaxFileBytes:
			lines = append(lines, unusable(cand, "larger than the "+mib(limits.MaxFileBytes)+" limit"))
			continue
		case total+size > limits.MaxBytesPerRun:
			lines = append(lines, unusable(cand, "over the "+mib(limits.MaxBytesPerRun)+" total for one request"))
			continue
		}
		mediaType, readable := SniffInput(body)
		if !readable {
			lines = append(lines, unusable(cand, "a "+chatMediaType(mediaType)+" file, which the agent cannot read"))
			continue
		}

		total += size
		files = append(files, run.InputFile{Name: cand.file.Name, Body: body, SourceMessageID: cand.postID})
		key := runv1.InputFileName(len(files), cand.file.Name)
		lines = append(lines, fmt.Sprintf("- %s/%s: %s, %s, %s, from %s\n",
			runv1.DirInputs, strings.TrimPrefix(key, runv1.StoragePrefixInputs),
			chatFileName(cand.file.Name), chatMediaType(mediaType), mib(size), chatPoster(cand.poster)))
	}
	if failed > 0 {
		// Counts only: a file's name is the customer's, like its bytes.
		c.log.Warn("some files attached to a chat message could not be downloaded",
			"failed", failed, "of", len(candidates))
	}

	var b strings.Builder
	b.WriteString(chatAttachHead)
	if c.fileReadDenied(ctx) {
		b.WriteString(chatAttachNoRead)
	}
	room := reserve - b.Len() - len(chatAttachTail) - len(fmt.Sprintf(chatAttachMoreLine, len(candidates))) - 2
	for _, line := range lines {
		if len(line) > room {
			unlisted++
			continue
		}
		b.WriteString(line)
		room -= len(line)
	}
	if unlisted > 0 {
		fmt.Fprintf(&b, chatAttachMoreLine, unlisted)
	}
	b.WriteString(chatAttachTail)
	return files, b.String()
}

// fileReadDenied reports whether the bot's role leaves claude-code or opencode without its
// Read tool, the one it opens images and PDFs with. Read from the role and the
// installation's ceiling, the same two policies admission intersects.
func (c *Chat) fileReadDenied(ctx context.Context) bool {
	agent := c.svc.defaults.Agent
	var deny []string
	if c.svc.defaults.ToolPolicyCeiling != nil {
		deny = append(deny, c.svc.defaults.ToolPolicyCeiling.Deny...)
	}
	if c.cfg.Role != "" {
		stored, err := c.svc.store.RoleByName(ctx, c.cfg.Role)
		if err == nil {
			if role, err := parseRole(stored); err == nil {
				if role.Agent != "" {
					agent = role.Agent
				}
				if role.ToolPolicy != nil {
					deny = append(deny, role.ToolPolicy.Deny...)
				}
			}
		}
	}
	// codex reads through its shell, so no single tool denies it the files;
	// claude-code and opencode read with a tool the deny list can name.
	if agent == runv1.AgentCodex {
		return false
	}
	for _, d := range deny {
		d = strings.TrimSpace(d)
		if d == "Read" || strings.HasPrefix(d, "Read(") {
			return true
		}
	}
	return false
}

// chatFileName is a file name as a prompt line carries it: one line, quoted,
// bounded. The name is the person's text and may say anything.
func chatFileName(name string) string {
	name = chatHeaderValue(strings.ReplaceAll(name, `"`, "'"))
	if len(name) > 160 {
		name = cutBytes(name, 160) + "…"
	}
	if name == "" {
		name = "(unnamed)"
	}
	return `"` + name + `"`
}

func chatPoster(username string) string {
	name := chatHeaderValue(username)
	if len(name) > 64 {
		name = cutBytes(name, 64)
	}
	if name == "" {
		return "someone"
	}
	return "@" + name
}

// chatMediaType is a sniffed type without its parameters, bounded.
func chatMediaType(mediaType string) string {
	base, _, _ := strings.Cut(mediaType, ";")
	base = strings.TrimSpace(base)
	if len(base) > 64 {
		base = base[:64]
	}
	return base
}

// humanBytes renders a size the way a person reads one.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", (n+512)>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
