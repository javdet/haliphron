package entrypoint

import (
	"context"
	"encoding/json"
	"mime"
	"path/filepath"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Output, persistence, and the ordering decision the whole retry path rests on.
//
// In the architecture the upload stood fifteenth, between pr and notify. That
// ordering — upload before notification — is correct and is kept, and it is not
// enough. One scenario shows why: the agent worked for forty minutes and twenty
// dollars, the push failed on a protected branch, and the second attempt read
// the checkpoint, saw run: ok, skipped the model, and found the result nowhere,
// because result.md and output.json had existed only in the filesystem of a pod
// that was already deleted.
//
// An idempotent retry promises not to pay for the model twice. That promise is
// not kept if what becomes durable is only what survived the very phases that
// fail. So persist runs immediately after the result exists, and before
// anything entitled to fail with a retryable class.

// phaseOutput wraps the agent's payload in the envelope and validates it
// against the node's schema when there is one.
func phaseOutput(_ context.Context, r *Run) error {
	if r.resumed {
		return skip("an earlier attempt already produced and stored the output")
	}

	names, err := CollectArtifacts(r.layout.Artifacts)
	if err != nil {
		return err
	}
	for _, name := range names {
		r.artifacts = append(r.artifacts, runv1.OutputArtifact{
			Path:        name,
			Key:         r.secrets.Bundle.KeyPrefix + "artifacts/" + name,
			ContentType: contentTypeFor(name),
		})
	}

	payload, readErr := ReadAgentPayload(r.layout.Output, r.nodeSchema)
	if readErr != nil {
		// The envelope is built anyway, with an empty payload and the status
		// the exit code implies. Failing here and writing nothing would leave
		// the post-mortem with no object under the fixed key — and "the agent's
		// output was rejected" is exactly the run somebody wants to look at.
		r.payload = &AgentPayload{Data: json.RawMessage("{}")}
		f := classify(readErr, runv1.ExitOutputInvalid)
		r.envelope = BuildEnvelope(r.cfg, r.payload, f.Code, r.summary, r.artifacts, r.clock())
		return readErr
	}

	r.payload = payload
	r.envelope = BuildEnvelope(r.cfg, payload, r.exitCodeSoFar(), r.summary, r.artifacts, r.clock())
	if len(r.nodeSchema) > 0 {
		r.logf("output.json validated against the node's schema")
	} else if !payload.Present {
		r.logf("no node schema and no %s: data is the empty object", r.layout.Output)
	}
	return nil
}

// phasePersist uploads everything the run has been paid for.
//
// Everything from here on can fail and be retried, and what the model produced
// is already durable by then and is never paid for twice.
func phasePersist(ctx context.Context, r *Run) error {
	if r.resumed {
		return skip("an earlier attempt already persisted the result")
	}

	// result.md first, and always — even when the agent produced no text. A
	// missing object under a fixed key breaks the read path for the backend and
	// the UI, and "the agent said nothing" is information too.
	if r.summary == "" {
		r.summary = ResultMarkdown(r.cfg, r.agent.Text, r.exitCodeSoFar(), r.failure, r.timings)
	}
	resultRef, err := r.uploader.Put(ctx, runv1.StorageKeyResult, []byte(r.summary), "text/markdown")
	if err != nil {
		return err
	}
	r.resultRef = resultRef

	if r.envelope == nil {
		r.envelope = BuildEnvelope(r.cfg, &AgentPayload{Data: json.RawMessage("{}")},
			r.exitCodeSoFar(), r.summary, r.artifacts, r.clock())
	}
	// The summary is only known once result.md exists, so the envelope carries
	// it from here rather than from the output phase.
	r.envelope.Summary = truncateSummary(r.summary)

	body, err := json.Marshal(r.envelope)
	if err != nil {
		return failWrap(runv1.ExitStorage, "OutputUnserialisable", err, "marshalling the envelope")
	}
	outputRef, err := r.uploader.Put(ctx, runv1.StorageKeyOutput, body, "application/json")
	if err != nil {
		return err
	}
	r.outputRef = outputRef

	r.uploadArtifacts(ctx)

	// The log up to this point, so that a pod which dies in the git phases
	// leaves a readable trail rather than the first two chunks.
	if err := r.log.Flush(ctx, r.uploader, r.checkpoint); err != nil {
		r.logf("log chunk upload failed during persist: %v", err)
	}

	r.logf("persisted: %s (%d bytes), %s (%d bytes), %d artifact(s)",
		resultRef.Key, resultRef.SizeBytes, outputRef.Key, outputRef.SizeBytes, len(r.artifacts))
	return nil
}

// uploadArtifacts ships what the agent left behind. A failure here is logged
// and not fatal: an artifact that did not upload is a missing attachment, and
// failing the run over it would discard the result as well.
func (r *Run) uploadArtifacts(ctx context.Context) {
	for i := range r.artifacts {
		art := &r.artifacts[i]
		path := filepath.Join(r.layout.Artifacts, filepath.FromSlash(art.Path))
		body, err := readCapped(path, maxAgentOutputBytes)
		if err != nil {
			r.logf("artifact %s could not be read and was not uploaded: %v", art.Path, err)
			continue
		}
		ref, err := r.uploader.PostUnder(ctx, "artifacts/", art.Path, body, art.ContentType)
		if err != nil {
			r.logf("artifact %s could not be uploaded: %v", art.Path, err)
			continue
		}
		art.Key = ref.Key
		art.SizeBytes = ref.SizeBytes
		art.SHA256 = ref.SHA256
	}
}

// contentTypeFor guesses from the extension. Only a hint for whoever downloads
// it later; nothing in this system branches on it.
func contentTypeFor(name string) string {
	if t := mime.TypeByExtension(filepath.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// phaseFinalize uploads the final log, the report and the last checkpoint.
//
// By the time it runs, everything that was paid for is already stored. What it
// adds is the log of the git phases, the report in its durable copy, and a
// checkpoint that reflects what actually happened rather than what was true
// before the push.
func phaseFinalize(ctx context.Context, r *Run) error {
	r.log.Stop()

	// The tail of the log, as one last chunk. Without it the chunks stop short
	// of agent.log and a reader who followed them to the end and switched to
	// the final object sees the git phases twice — or, on a resumed attempt
	// that flushed nothing, sees the chunk counter stand still and the next
	// attempt overwrite what this one wrote.
	if err := r.log.Flush(ctx, r.uploader, r.checkpoint); err != nil {
		r.logf("the last log chunk could not be uploaded: %v", err)
	}

	// The concatenation of the same bytes the chunks carried. A reader who has
	// followed the chunks to the end and switched to the final log must see
	// neither a gap nor a repetition.
	logRef, err := r.uploader.Put(ctx, runv1.StorageKeyAgentLog, r.log.Bytes(), "text/plain")
	if err != nil {
		// Reported and survived: the chunks are already there, and the run's
		// result does not depend on the tidy copy.
		r.logf("the final log could not be uploaded: %v", err)
	} else {
		r.logRef = logRef
	}

	r.report = r.buildReport()
	body, err := json.Marshal(r.report)
	if err != nil {
		return failWrap(runv1.ExitStorage, "ReportUnserialisable", err, "marshalling the completion report")
	}
	// Kept verbatim so that the object in storage and the webhook body are the
	// same bytes. Either of them may turn out to be the one that survived, and
	// a difference between them is a question nobody can answer afterwards.
	r.reportBody = body

	if _, err := r.uploader.Put(ctx, runv1.StorageKeyCompletion, body, "application/json"); err != nil {
		return err
	}

	// The log reference only exists once the final log has been uploaded, which
	// is a few lines above the report that names it. Rewriting the object once
	// is cheaper than reordering the phase: the alternative is to upload the
	// log after the report, and then a pod killed in between leaves a report
	// naming a log that is not there.
	if r.logRef != nil {
		r.report.LogRef = r.logRef
		if body, err := json.Marshal(r.report); err == nil {
			r.reportBody = body
			if _, err := r.uploader.Put(ctx, runv1.StorageKeyCompletion, body, "application/json"); err != nil {
				r.logf("the completion report could not be updated with its log reference: %v", err)
			}
		}
	}
	return nil
}
