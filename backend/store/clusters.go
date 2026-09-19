package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Clusters and the bootstrap tokens they register with.
//
// There is not one credential in this table that grants access into a cluster,
// and there will not be: what is stored is the public half of a pair the
// controller generated for itself. Revocation is a status change and takes
// effect within the five-minute life of a token, which is why there is no
// revocation list — the row is read on every request anyway.

// Cluster is a registered cluster as the backend holds it.
type Cluster struct {
	ID                runv1.ULID
	Name              string
	Labels            map[string]string
	Status            string
	KeyID             string
	PublicKey         []byte
	AgentNamespace    string
	ControllerVersion string
	K8sVersion        string
	Runtimes          []runv1.AgentType
	CRDVersions       []string
	CapacitySlots     int32
	FreeSlots         int32
	QuotaExhausted    bool
	RegisteredAt      time.Time
	LastHeartbeatAt   *time.Time
	RevokedAt         *time.Time
	RevokedReason     string
}

// Revoked reports whether this cluster may still act. Read from the row on
// every authenticated request, which is what makes revocation immediate enough
// not to need a list of its own.
func (c Cluster) Revoked() bool { return c.Status == "Revoked" }

// Registration is what /register was asked for.
type Registration struct {
	BootstrapToken string
	Name           string
	Labels         map[string]string
	KeyID          string
	PublicKey      []byte

	ControllerVersion string
	AgentNamespace    string
	K8sVersion        string
	Runtimes          []runv1.AgentType
	CRDVersions       []string
	CapacitySlots     int32
}

// Registration failures, told apart because the controller must do different
// things about them: a bad token is fatal, a taken name needs a human, and a
// repeat with a different key is a stolen token being replayed.
var (
	ErrBootstrapTokenInvalid  = errors.New("store: bootstrap token is unknown, expired or revoked")
	ErrBootstrapTokenConsumed = errors.New("store: bootstrap token already spent by another key")
	ErrClusterNameTaken       = errors.New("store: cluster name is taken")
)

// Register exchanges a bootstrap token for an identity, idempotently on
// (token, public key).
//
// The idempotency is not decoration. A controller that received a 200 and
// crashed before writing the clusterID into its Secret would otherwise be
// incurable: the token is spent and there is no identity, and repairing it
// takes a human with the UI. Keyed on the pair, the retry after a restart
// returns the same row.
func (s *Store) Register(ctx context.Context, reg Registration) (Cluster, error) {
	digest := sha256.Sum256([]byte(reg.BootstrapToken))

	var out Cluster
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var (
			tokenID  string
			maxUses  int
			uses     int
			expires  time.Time
			revoked  sql.NullTime
			existing sql.NullString
		)
		// FOR UPDATE on the token row is what serialises two controllers
		// racing on the same token: without it both read uses=0 and both
		// register.
		err := tx.QueryRowContext(ctx, `
			SELECT id, max_uses, uses, expires_at, revoked_at
			FROM cluster_bootstrap_tokens
			WHERE token_sha256 = $1
			FOR UPDATE`, digest[:]).Scan(&tokenID, &maxUses, &uses, &expires, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBootstrapTokenInvalid
		}
		if err != nil {
			return fmt.Errorf("store: read bootstrap token: %w", err)
		}
		if revoked.Valid || expires.Before(time.Now()) {
			return ErrBootstrapTokenInvalid
		}

		// The pair is unique in the schema, so this lookup and the insert
		// below are the two halves of one idempotency rule.
		err = tx.QueryRowContext(ctx, `
			SELECT id FROM clusters
			WHERE bootstrap_token_id = $1 AND public_key = $2`,
			tokenID, reg.PublicKey).Scan(&existing)
		switch {
		case err == nil:
			cluster, err := readCluster(ctx, tx, runv1.ULID(existing.String))
			if err != nil {
				return err
			}
			out = cluster
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: look up registration: %w", err)
		}

		if uses >= maxUses {
			return ErrBootstrapTokenConsumed
		}

		labels, err := json.Marshal(nonNilLabels(reg.Labels))
		if err != nil {
			return fmt.Errorf("store: encode cluster labels: %w", err)
		}
		id := newID()

		_, err = tx.ExecContext(ctx, `
			INSERT INTO clusters (id, name, labels, status, key_id, public_key,
			                      bootstrap_token_id, agent_namespace, controller_version,
			                      k8s_version, runtimes, crd_versions,
			                      capacity_slots, free_slots)
			VALUES ($1, $2, $3::jsonb, 'Active', $4, $5, $6, $7, $8, $9,
			        $10::text[]::agent_type[], $11::text[], $12, $12)`,
			id, reg.Name, labels, reg.KeyID, reg.PublicKey, tokenID,
			reg.AgentNamespace, reg.ControllerVersion, nullString(reg.K8sVersion),
			agentTypeArray(reg.Runtimes), textArray(reg.CRDVersions), reg.CapacitySlots)
		if err != nil {
			if isUniqueViolation(err, "clusters_name_key") {
				return ErrClusterNameTaken
			}
			if isUniqueViolation(err, "clusters_key_id_key") || isUniqueViolation(err, "clusters_public_key_key") {
				// The same key under a different bootstrap token. Registering
				// it again would give one keypair two identities, and the
				// subject check would be the only thing telling them apart.
				return ErrBootstrapTokenConsumed
			}
			return fmt.Errorf("store: insert cluster: %w", err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE cluster_bootstrap_tokens SET uses = uses + 1 WHERE id = $1`, tokenID); err != nil {
			return fmt.Errorf("store: spend bootstrap token: %w", err)
		}

		cluster, err := readCluster(ctx, tx, id)
		if err != nil {
			return err
		}
		out = cluster
		return nil
	})
	return out, err
}

// ClusterByKeyID finds the cluster a JWT's kid belongs to. This is the lookup
// every authenticated request makes, and the reason the column is unique on
// its own.
func (s *Store) ClusterByKeyID(ctx context.Context, kid string) (Cluster, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM clusters WHERE key_id = $1`, kid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, ErrNotFound
	}
	if err != nil {
		return Cluster{}, fmt.Errorf("store: look up key %s: %w", kid, err)
	}
	return readCluster(ctx, s.db, runv1.ULID(id))
}

// ClusterByID reads one cluster.
func (s *Store) ClusterByID(ctx context.Context, id runv1.ULID) (Cluster, error) {
	return readCluster(ctx, s.db, id)
}

// ListClusters returns every cluster, newest registration first. The table
// holds tens of rows, so there is no pagination and no index to support one.
func (s *Store) ListClusters(ctx context.Context) ([]Cluster, error) {
	rows, err := s.db.QueryContext(ctx, clusterColumns+` FROM clusters ORDER BY registered_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list clusters: %w", err)
	}
	defer rows.Close()

	var out []Cluster
	for rows.Next() {
		c, err := scanCluster(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClusterFacts is what a heartbeat tells the backend about a cluster.
type ClusterFacts struct {
	FreeSlots     int32
	CapacitySlots int32

	K8sVersion     string
	NodeCount      *int32
	Runtimes       []runv1.AgentType
	CRDVersions    []string
	QuotaExhausted bool

	ControllerVersion string
}

// RecordHeartbeat updates liveness and whatever drifted.
//
// It writes free_slots and last_heartbeat_at on every call — once per cluster
// every ten seconds — which is exactly why there is no index on either column:
// an index on something written that often costs more than the scan it saves,
// and it would break the HOT update as well.
func (s *Store) RecordHeartbeat(ctx context.Context, id runv1.ULID, facts ClusterFacts) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE clusters SET
			free_slots         = $2,
			capacity_slots     = CASE WHEN $3 > 0 THEN $3 ELSE capacity_slots END,
			k8s_version        = COALESCE($4, k8s_version),
			node_count         = COALESCE($5, node_count),
			runtimes           = CASE WHEN cardinality($6::text[]) > 0
			                          THEN $6::text[]::agent_type[] ELSE runtimes END,
			crd_versions       = CASE WHEN cardinality($7::text[]) > 0
			                          THEN $7::text[] ELSE crd_versions END,
			quota_exhausted    = $8,
			controller_version = COALESCE($9, controller_version),
			status             = CASE WHEN status = 'Revoked' THEN status ELSE 'Active' END,
			last_heartbeat_at  = now()
		WHERE id = $1`,
		id, facts.FreeSlots, facts.CapacitySlots,
		nullString(facts.K8sVersion), nullInt32(facts.NodeCount),
		agentTypeArray(facts.Runtimes), textArray(facts.CRDVersions),
		facts.QuotaExhausted, nullString(facts.ControllerVersion))
	if err != nil {
		return fmt.Errorf("store: record heartbeat for %s: %w", id, err)
	}
	return nil
}

// RecordLease notes that this cluster was handed work, for the operator view.
func (s *Store) RecordLease(ctx context.Context, id runv1.ULID) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clusters SET last_lease_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: record lease for %s: %w", id, err)
	}
	return nil
}

// MarkStaleClusters moves clusters that stopped reporting to Unreachable and
// returns the ones that changed. Placement stops choosing them; their runs are
// dealt with by the lease expiry scanner, which is a different deadline with a
// different consequence.
func (s *Store) MarkStaleClusters(ctx context.Context, staleAfter time.Duration) ([]runv1.ULID, error) {
	rows, err := s.db.QueryContext(ctx, `
		UPDATE clusters SET status = 'Unreachable'
		WHERE status = 'Active'
		  AND last_heartbeat_at IS NOT NULL
		  AND last_heartbeat_at < now() - make_interval(secs => $1)
		RETURNING id`, staleAfter.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: mark stale clusters: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// RevokeCluster ends a cluster's ability to act. Its runs are left alone: they
// are the record of work that happened, and deciding what to do with the ones
// still in flight is an operator's call, not a side effect of revocation.
func (s *Store) RevokeCluster(ctx context.Context, id runv1.ULID, reason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE clusters SET status = 'Revoked', revoked_at = now(), revoked_reason = $2
		WHERE id = $1 AND status <> 'Revoked'`, id, nullString(reason))
	if err != nil {
		return fmt.Errorf("store: revoke cluster %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it does not exist or it is already revoked; the caller asked
		// for a state and the state holds.
		if _, err := s.ClusterByID(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// BootstrapToken is a one-time credential for registering a cluster.
type BootstrapToken struct {
	ID        runv1.ULID
	Name      string
	Token     string // returned once, at creation, and never stored
	ExpiresAt time.Time
	MaxUses   int
	Uses      int
	CreatedBy string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// CreateBootstrapToken mints a token and stores only its digest. A dump of
// this table that contained the token itself would be a way to register a
// rogue controller.
func (s *Store) CreateBootstrapToken(ctx context.Context, name, createdBy string, ttl time.Duration, maxUses int) (BootstrapToken, error) {
	if maxUses <= 0 {
		maxUses = 1
	}
	secret, err := randomToken()
	if err != nil {
		return BootstrapToken{}, err
	}
	token := clusterv1.BootstrapTokenPrefix + secret
	digest := sha256.Sum256([]byte(token))

	out := BootstrapToken{
		ID: newID(), Name: name, Token: token,
		ExpiresAt: time.Now().Add(ttl), MaxUses: maxUses, CreatedBy: createdBy,
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO cluster_bootstrap_tokens (id, name, token_sha256, expires_at, max_uses, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`,
		out.ID, name, digest[:], out.ExpiresAt, maxUses, createdBy).Scan(&out.CreatedAt)
	if err != nil {
		return BootstrapToken{}, fmt.Errorf("store: create bootstrap token: %w", err)
	}
	return out, nil
}

// ListBootstrapTokens returns the registration record: which token admitted
// which cluster, issued by whom, and when.
func (s *Store) ListBootstrapTokens(ctx context.Context) ([]BootstrapToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, expires_at, max_uses, uses, created_by, created_at, revoked_at
		FROM cluster_bootstrap_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list bootstrap tokens: %w", err)
	}
	defer rows.Close()

	var out []BootstrapToken
	for rows.Next() {
		var (
			t       BootstrapToken
			revoked sql.NullTime
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.ExpiresAt, &t.MaxUses, &t.Uses,
			&t.CreatedBy, &t.CreatedAt, &revoked); err != nil {
			return nil, fmt.Errorf("store: scan bootstrap token: %w", err)
		}
		t.RevokedAt = timePtr(revoked)
		out = append(out, t)
	}
	return out, rows.Err()
}

const clusterColumns = `
	SELECT id, name, labels, status, key_id, public_key, agent_namespace,
	       controller_version, k8s_version, runtimes::text[], crd_versions,
	       capacity_slots, free_slots, quota_exhausted,
	       registered_at, last_heartbeat_at, revoked_at, revoked_reason`

type rowScanner interface {
	Scan(dest ...any) error
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readCluster(ctx context.Context, q queryer, id runv1.ULID) (Cluster, error) {
	row := q.QueryRowContext(ctx, clusterColumns+` FROM clusters WHERE id = $1`, id)
	c, err := scanCluster(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Cluster{}, ErrNotFound
	}
	return c, err
}

func scanCluster(row rowScanner) (Cluster, error) {
	var (
		c          Cluster
		labels     []byte
		k8s        sql.NullString
		runtimes   []string
		crds       []string
		heartbeat  sql.NullTime
		revokedAt  sql.NullTime
		revokedWhy sql.NullString
	)
	err := row.Scan(&c.ID, &c.Name, &labels, &c.Status, &c.KeyID, &c.PublicKey,
		&c.AgentNamespace, &c.ControllerVersion, &k8s, pgArray(&runtimes), pgArray(&crds),
		&c.CapacitySlots, &c.FreeSlots, &c.QuotaExhausted,
		&c.RegisteredAt, &heartbeat, &revokedAt, &revokedWhy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cluster{}, err
		}
		return Cluster{}, fmt.Errorf("store: scan cluster: %w", err)
	}

	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &c.Labels); err != nil {
			return Cluster{}, fmt.Errorf("store: decode cluster labels: %w", err)
		}
	}
	c.K8sVersion = k8s.String
	c.RevokedReason = revokedWhy.String
	c.LastHeartbeatAt = timePtr(heartbeat)
	c.RevokedAt = timePtr(revokedAt)
	for _, rt := range runtimes {
		c.Runtimes = append(c.Runtimes, runv1.AgentType(rt))
	}
	c.CRDVersions = crds
	return c, nil
}

func nonNilLabels(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}
