package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/store"
)

// The relay's landing point: bytes a controller forwarded on behalf of a pod.
//
// This is the half of the design that makes object storage optional. The pod
// posted an object to its controller, the controller wrote it to its own volume
// before acknowledging, and now it is forwarding it here with the usual retry
// and backoff. The backend writes it under the fixed key layout onto its PVC,
// and only then is the controller entitled to forget its copy.
//
// The cost of this path is real and was accepted deliberately: the backend is
// in the artifact data path, and gigabytes of logs pass through it. What bounds
// it is a per-run byte cap enforced by the controller — so the transfer is not
// paid for twice — plus the log chunking that already existed, which relays a
// log as it is produced rather than as one final push.

// IngestArtifact writes one relayed object.
//
// The key the controller sends is relative to the run, and the run prefix is
// stamped here from the envelope this call authenticated against. That is the
// third place the same rule is applied — the controller stamped it from the CR,
// the disk store refuses a key that escapes its root — and the repetition is
// deliberate: the bytes originate in the least trusted component in the system,
// and "which prefix does this land under" is the one question it must never get
// to answer.
func (s *Service) IngestArtifact(ctx context.Context, cluster store.Cluster,
	req clusterv1.ArtifactIngestRequest, body io.Reader) (clusterv1.ArtifactIngestResponse, error) {

	key, err := runv1.ArtifactKey(req.Key)
	if err != nil {
		return clusterv1.ArtifactIngestResponse{}, &clusterv1.Problem{
			Title: "invalid artifact key", Status: 400, Detail: err.Error(),
			Code: clusterv1.CodeInvalidRequest, Action: clusterv1.ActionFatal, RunID: req.RunID,
		}
	}

	// Ownership before bytes. A cluster that lost this run must not be able to
	// overwrite the result the cluster that now holds it produced, and finding
	// that out after streaming a gigabyte would be finding it out too late.
	if _, err := s.store.VerifyOwnership(ctx, req.RunID, cluster.ID, req.Epoch); err != nil {
		return clusterv1.ArtifactIngestResponse{}, s.problemFor(req.RunID, err)
	}

	full := artifacts.Key(req.RunID, key)

	// The digest is computed on the way past and compared after. Verifying
	// before writing would mean buffering the object, which is the one thing
	// this path exists not to do; verifying after means a bad object can exist
	// under a temporary name, which the disk store already handles — it renames
	// into place, and a failed verification simply never reaches the rename.
	sum := sha256.New()
	ref, err := s.artifacts.PutStream(ctx, full, io.TeeReader(body, sum), req.ContentType)
	if err != nil {
		return clusterv1.ArtifactIngestResponse{}, fmt.Errorf("store artifact %s: %w", full, err)
	}

	if want := strings.ToLower(req.SHA256); want != "" && want != hex.EncodeToString(sum.Sum(nil)) {
		// A truncated relay. Refused rather than kept, because a half-written
		// result.md under the right key is worse than none at all: the
		// CompletedWithoutResult recovery path would read it and believe it.
		if delErr := s.artifacts.Delete(ctx, full); delErr != nil {
			s.log.Error("could not remove a truncated artifact",
				"run", req.RunID, "key", full, "error", delErr)
		}
		return clusterv1.ArtifactIngestResponse{}, &clusterv1.Problem{
			Title: "the artifact does not match its digest", Status: 400,
			Detail: fmt.Sprintf("%s hashes to %s and the controller said %s", key, ref.SHA256, want),
			// Retry, not fatal: the likely cause is a connection that dropped
			// mid-transfer, and the controller still holds the whole object in
			// its spool.
			Code: clusterv1.CodeInvalidRequest, Action: clusterv1.ActionRetry, RunID: req.RunID,
		}
	}

	// result.md is the one key worth recording a pointer to, because it is what
	// the UI opens and what runs.result_ref names. The rest are found by their
	// fixed keys under the run's prefix and need no row.
	if key == runv1.StorageKeyResult {
		if err := s.store.SetResultRef(ctx, req.RunID, artifacts.Ref(s.artifacts, full)); err != nil {
			return clusterv1.ArtifactIngestResponse{}, err
		}
	}

	s.log.Info("artifact relayed",
		"run", req.RunID, "cluster", cluster.ID, "key", key, "bytes", ref.SizeBytes)
	return clusterv1.ArtifactIngestResponse{RunID: req.RunID, Ref: ref}, nil
}
