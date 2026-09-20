package entrypoint

import (
	"context"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// The pod's side of the ArtifactStore port: two ways to make a result durable,
// and no way to read anything back.
//
// There used to be reads. The prompt and the checkpoint were objects this
// process fetched with presigned GETs, and between them they made an object
// store a prerequisite for *starting* a run rather than for finishing one. Both
// are gone — the prompt arrives in the environment, the checkpoint arrives in
// the environment — and with them went exit code 21's most common cause and a
// whole class of "the run failed and the reason was a URL expiry".
//
// What remains is writing, and there are two ways to do it:
//
//	relay         POST to the controller's Service, which spools and forwards
//	object-store  presigned PUT and POST straight to S3 or MinIO
//
// The pod holds no storage credential in either. In relay mode it addresses no
// store at all and cannot name another run's prefix, because the controller
// stamps the prefix from the CR. In object-store mode a presigned link bounds it
// to its own.
//
// Every failure on this path is exit 21, class infra, retryable — and that
// classing is the point of the code existing at all. In relay mode the
// controller was restarting and the next attempt finds it back; in object-store
// mode the signature had expired and the controller mints a fresh bundle before
// the next attempt. Either way it is the one failure the cluster repairs by
// itself, so it is obliged to be retryable.

// Uploader is what a phase uses to make something durable.
//
// Deliberately narrow. Two methods, no reads, no listing, no deletes: a pod that
// could list its own prefix could confirm what a previous attempt did, and the
// checkpoint that question belongs to is now a column in the control plane
// rather than an object here.
type Uploader interface {
	// Mode is which half is in force, for the log line that makes a failure
	// diagnosable without guessing at the configuration.
	Mode() runv1.ArtifactMode

	// Put writes one of the keys fixed by the layout: result.md, output.json,
	// completion.json, logs/agent.log.
	Put(ctx context.Context, key string, body []byte, contentType string) (*runv1.ObjectRef, error)

	// PostUnder writes a key whose name was not known in advance — a log chunk,
	// or a file the agent chose to keep. suffix is relative to prefix, and
	// prefix is relative to the run.
	PostUnder(ctx context.Context, prefix, suffix string, body []byte, contentType string) (*runv1.ObjectRef, error)

	// Describe is what the startup log says about where results are going. In
	// object-store mode it includes the bundle's expiry, which is the first
	// suspect when an upload fails at minute fifty.
	Describe() string
}

// NewUploader picks the half this run uses.
//
// The mode comes from the environment rather than from the presence of a
// presigned bundle. Inferring it would make a Secret written without
// presigned.json look like a deliberate choice of relay mode rather than the
// defect it would be, and a phase that fails should be able to say which path
// it was taking.
func NewUploader(cfg *Config, secrets *Secrets, redactor *Redactor, callback *Callback) (Uploader, error) {
	if cfg.ArtifactMode == runv1.ArtifactModeObjectStore {
		if secrets.Bundle == nil {
			return nil, fail(runv1.ExitConfig, "MissingSecret",
				"%s says object-store mode and the secret mount has no %s: "+
					"this pod has nowhere to put its result",
				runv1.EnvArtifactMode, runv1.SecretKeyPresigned)
		}
		return NewStorage(*secrets.Bundle, redactor), nil
	}
	if callback == nil {
		return nil, fail(runv1.ExitConfig, "MissingConfiguration",
			"relay mode needs a callback URL and none was configured")
	}
	return &Relay{callback: callback, redactor: redactor}, nil
}
