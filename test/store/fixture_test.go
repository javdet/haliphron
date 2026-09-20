package store

import (
	"crypto/rand"
	"database/sql"
	"testing"
	"time"
)

// Crockford base32, the alphabet ULIDs are written in: I, L, O and U are
// omitted so that a transcribed identifier cannot turn into a different valid
// one.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newULID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 26)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	out := make([]byte, 26)
	for i, v := range b {
		out[i] = crockford[int(v)%len(crockford)]
	}
	return string(out)
}

type fixture struct {
	clusterID string
	runID     string
}

// seed inserts one cluster and one queued run, the smallest state in which the
// lease protocol has anything to do.
func seed(t *testing.T, conn *sql.DB) fixture {
	t.Helper()
	cluster := newCluster(t, conn, "test-cluster")
	run := newQueuedRun(t, conn, cluster)
	return fixture{clusterID: cluster, runID: run}
}

func newCluster(t *testing.T, conn *sql.DB, name string) string {
	t.Helper()
	tokenID, clusterID := newULID(t), newULID(t)

	_, err := conn.Exec(`
		INSERT INTO cluster_bootstrap_tokens (id, name, token_sha256, expires_at, created_by)
		VALUES ($1, $2, sha256($3::bytea), now() + interval '1 hour', 'test')`,
		tokenID, name+"-token", []byte(tokenID))
	if err != nil {
		t.Fatalf("insert bootstrap token: %v", err)
	}

	_, err = conn.Exec(`
		INSERT INTO clusters (id, name, status, key_id, public_key, bootstrap_token_id,
		                      agent_namespace, controller_version, runtimes,
		                      capacity_slots, free_slots, last_heartbeat_at)
		VALUES ($1, $2, 'Active', $3, $4, $5, 'haliphron-agents', '1.0.0',
		        ARRAY['claude-code']::agent_type[], 10, 10, now())`,
		clusterID, name, "kid-"+name, randomKey(t), tokenID)
	if err != nil {
		t.Fatalf("insert cluster: %v", err)
	}
	return clusterID
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("random: %v", err)
	}
	return k
}

func newQueuedRun(t *testing.T, conn *sql.DB, clusterID string) string {
	t.Helper()
	runID := newULID(t)
	// The prompt is a column and is NOT NULL, because a run without a task is
	// not a run. The digest is taken over the same bytes, as admission does, so
	// a fixture cannot produce a row whose two halves disagree.
	prompt := "do the thing for " + runID
	_, err := conn.Exec(`
		INSERT INTO runs (id, created_by, created_via, spec, prompt, prompt_sha256,
		                  agent, model, timeout_seconds, cluster_id)
		VALUES ($1, 'test', 'api', $2::jsonb, $3::text, sha256($4::bytea),
		        'claude-code', 'anthropic/claude-opus-5', 3600, $5)`,
		runID, minimalSpec, prompt, []byte(prompt), clusterID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return runID
}

// Enough of a RenderedRunSpec to be a real object. The schema checks that it is
// an object and nothing more: the shape is the CRD's contract, validated by the
// API server and by test/contract, and duplicating that validation here would
// be a second opinion nobody asked for.
const minimalSpec = `{
  "agent": "claude-code",
  "model": "anthropic/claude-opus-5",
  "image": "ghcr.io/automagicops/agent-runtime@sha256:0000",
  "promptSHA256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "repo": {"url": "https://github.com/example/repo", "provider": "github"},
  "runtime": {"timeoutSeconds": 3600}
}`

// leaseOne runs the contract lease statement for one run and reports whether it
// got anything.
func leaseOne(t *testing.T, conn *sql.DB, clusterID string, ackSecs, ttlSecs int) (runID string, epoch int64, ok bool) {
	t.Helper()
	rows, err := conn.Query(leaseSQL(t), clusterID, []string{"claude-code"}, 1, ackSecs, ttlSecs)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			attempt, priority, timeout int
			spec                       []byte
			prompt                     string
			promptSHA256               []byte
			ackDeadline, leaseDeadline time.Time
		)
		// The prompt travels in the lease now, which is what took the artifact
		// store off the path to starting a run.
		if err := rows.Scan(&runID, &epoch, &attempt, &priority, &spec,
			&prompt, &promptSHA256,
			&ackDeadline, &leaseDeadline, &timeout); err != nil {
			t.Fatalf("scan lease: %v", err)
		}
		if prompt == "" {
			t.Fatalf("the lease of %s carries no prompt", runID)
		}
		ok = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("lease rows: %v", err)
	}
	return runID, epoch, ok
}
