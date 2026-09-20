package entrypoint

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The log, and why it is not `kubectl logs -f`.
//
// The decision was made against live streaming: chunks work identically for a
// cluster on a laptop and for one behind NAT, and they outlive the pod, which
// `kubectl logs` does not. The cost is upload latency, and that is a parameter.
//
// The entrypoint tees the agent's combined stream to a file and ships the
// increments through whichever half of the Uploader port this run is using. The
// key is logs/chunks/{seq}.log with seq six digits and leading zeros — the
// padding is not cosmetic, because a listing is lexicographic over a bucket and
// over a directory alike, and without it the tenth chunk sorts between the
// first and the second. Each attempt numbers within a block of its own, so the
// second attempt of a run does not overwrite the first one's log and leave the
// reader with two runs spliced seamlessly into one.

// chunkSizeTrigger ships a chunk early when the log has grown by this much,
// whichever comes first with the interval. A chatty agent should not be able to
// accumulate a hundred megabytes of unshipped log in front of an OOM kill.
const chunkSizeTrigger = 1 << 20

// LogSink is the rolling log. Safe for concurrent use: the agent's stdout and
// stderr both write to it, and the uploader reads from it on its own goroutine.
type LogSink struct {
	mu       sync.Mutex
	buf      []byte
	uploaded int
	path     string
	file     *os.File

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// NewLogSink opens the rolling log under the entrypoint's private directory —
// outside the work tree and outside anything the agent is pointed at, so that a
// prompt injection which talks the agent into rewriting "its log" rewrites a
// file nobody reads again.
func NewLogSink(path string) (*LogSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, failWrap(runv1.ExitConfig, "LogUnwritable", err, "opening %s", path)
	}
	return &LogSink{path: path, file: f}, nil
}

// Write appends to the log. It never fails the caller: a full disk must not
// take down the process that still has to persist a paid-for result, and the
// truncated log is the lesser loss.
func (l *LogSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	if l.file != nil {
		_, _ = l.file.Write(p)
	}
	return len(p), nil
}

// Printf writes one of the entrypoint's own lines, timestamped. The
// entrypoint's narration and the agent's output share one stream on purpose:
// reading them apart is exactly the work a person does when a run went wrong.
func (l *LogSink) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(l, "%s haliphron: %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

// Bytes is the whole log so far.
func (l *LogSink) Bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf...)
}

// pending returns the bytes written since the last chunk.
func (l *LogSink) pending() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.buf[l.uploaded:]...)
}

// commit marks n bytes as shipped.
func (l *LogSink) commit(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.uploaded += n
}

// Flush ships whatever has accumulated as the next chunk.
//
// A failed chunk upload is reported and not retried here, and it is emphatically
// not fatal: losing a fragment of log is a nuisance, and losing the run because
// a fragment of log could not be uploaded is a fault. The final agent.log in
// finalize carries the same bytes anyway.
func (l *LogSink) Flush(ctx context.Context, up Uploader, cp *Checkpoint) error {
	body := l.pending()
	if len(body) == 0 {
		return nil
	}
	seq := cp.nextChunk()
	if _, err := up.PostUnder(ctx, runv1.StoragePrefixChunks,
		fmt.Sprintf("%06d.log", seq), body, "text/plain"); err != nil {
		return err
	}
	l.commit(len(body))
	return nil
}

// Start ships chunks in the background until Stop. The uploader runs for the
// whole run rather than only during the agent phase: a clone of a monorepo
// takes minutes and produces output, and a pod evicted during it should not
// leave the reader with nothing.
func (l *LogSink) Start(ctx context.Context, up Uploader, cp *Checkpoint, interval time.Duration) {
	l.stop = make(chan struct{})
	l.done = make(chan struct{})

	go func() {
		defer close(l.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		size := time.NewTicker(time.Second)
		defer size.Stop()

		for {
			select {
			case <-l.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.tryFlush(ctx, up, cp)
			case <-size.C:
				l.mu.Lock()
				grown := len(l.buf) - l.uploaded
				l.mu.Unlock()
				if grown >= chunkSizeTrigger {
					l.tryFlush(ctx, up, cp)
				}
			}
		}
	}()
}

func (l *LogSink) tryFlush(ctx context.Context, up Uploader, cp *Checkpoint) {
	if err := l.Flush(ctx, up, cp); err != nil {
		// Deliberately only to the local log. Writing it anywhere else would
		// mean an upload failure that produces more to upload.
		l.Printf("log chunk upload failed: %v", err)
	}
}

// Stop halts the uploader and waits for it. Idempotent: finalize calls it, and
// so does the signal path, and the two can race.
func (l *LogSink) Stop() {
	l.once.Do(func() {
		if l.stop != nil {
			close(l.stop)
			<-l.done
		}
	})
}

// Close stops the uploader and closes the file.
func (l *LogSink) Close() error {
	l.Stop()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
