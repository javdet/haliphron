package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Secrets, in the two modes the SecretResolver port has.
//
// managed: the value is encrypted here under a key generated for the row, and
// that key is stored wrapped by a KEK which lives in the environment, a file or
// a KMS and never in this database. The property being bought is narrow and
// worth stating plainly: a database dump is not a credential leak.
//
// referenced: the value is in the customer's Vault or External Secrets, and the
// row holds only the pointer. The backend never sees it, and resolving it is
// the caller's business.
//
// Two layers rather than one because of rotation: re-keying an installation
// means unwrapping and rewrapping N small keys, not decrypting and
// re-encrypting N secrets, and the kek_id column is what tells the two
// generations apart while both exist.

// Secret kinds, as the secret_kind domain spells them.
const (
	SecretManaged    = "managed"
	SecretReferenced = "referenced"
)

// Secret is a stored secret without its value.
type Secret struct {
	ID        runv1.ULID
	Name      string
	Kind      string
	RefURI    string
	KEKID     string
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
	RotatedAt *time.Time
}

// ErrNoKEK is a managed secret asked for by an installation that configured no
// key encryption key. It is a configuration failure and is named as one: the
// alternative is storing a credential in the clear because a variable was
// unset.
var ErrNoKEK = errors.New("store: no key encryption key is configured; managed secrets are unavailable")

// ErrSecretNotManaged is a referenced secret asked to produce its value. The
// backend never had it.
var ErrSecretNotManaged = errors.New("store: secret is a reference; its value lives outside this system")

// SetKEK installs the key encryption key. It is 32 bytes, and it is handed to
// the store rather than read by it so that where it comes from — an env var, a
// mounted file, a KMS unwrap at startup — stays a deployment decision.
func (s *Store) SetKEK(id string, key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("store: key encryption key must be 32 bytes, got %d", len(key))
	}
	s.kekID, s.kek = id, key
	return nil
}

// PutManagedSecret encrypts a value and stores it.
func (s *Store) PutManagedSecret(ctx context.Context, name string, value []byte, by string) (Secret, error) {
	if len(s.kek) == 0 {
		return Secret{}, ErrNoKEK
	}

	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return Secret{}, fmt.Errorf("store: generate data key: %w", err)
	}
	nonce, ciphertext, err := sealAESGCM(dek, value)
	if err != nil {
		return Secret{}, err
	}
	wrapNonce, wrapped, err := sealAESGCM(s.kek, dek)
	if err != nil {
		return Secret{}, err
	}

	var out Secret
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO secrets (id, name, kind, kek_id, dek_wrapped, nonce, ciphertext, created_by)
		VALUES ($1, $2, 'managed', $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			kind = 'managed', kek_id = EXCLUDED.kek_id, dek_wrapped = EXCLUDED.dek_wrapped,
			nonce = EXCLUDED.nonce, ciphertext = EXCLUDED.ciphertext,
			ref_uri = NULL, rotated_at = now()
		RETURNING id, name, kind, kek_id, created_by, created_at, updated_at, rotated_at`,
		newID(), name, s.kekID, append(wrapNonce, wrapped...), nonce, ciphertext, by).
		Scan(&out.ID, &out.Name, &out.Kind, &out.KEKID, &out.CreatedBy,
			&out.CreatedAt, &out.UpdatedAt, nullTimeScanner(&out.RotatedAt))
	if err != nil {
		return Secret{}, fmt.Errorf("store: put managed secret %s: %w", name, err)
	}
	return out, nil
}

// PutReferencedSecret stores a pointer to a value the backend never sees.
func (s *Store) PutReferencedSecret(ctx context.Context, name, uri, by string) (Secret, error) {
	var out Secret
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO secrets (id, name, kind, ref_uri, created_by)
		VALUES ($1, $2, 'referenced', $3, $4)
		ON CONFLICT (tenant_id, name) DO UPDATE SET
			kind = 'referenced', ref_uri = EXCLUDED.ref_uri,
			kek_id = NULL, dek_wrapped = NULL, nonce = NULL, ciphertext = NULL,
			rotated_at = now()
		RETURNING id, name, kind, COALESCE(ref_uri, ''), created_by, created_at, updated_at, rotated_at`,
		newID(), name, uri, by).
		Scan(&out.ID, &out.Name, &out.Kind, &out.RefURI, &out.CreatedBy,
			&out.CreatedAt, &out.UpdatedAt, nullTimeScanner(&out.RotatedAt))
	if err != nil {
		return Secret{}, fmt.Errorf("store: put referenced secret %s: %w", name, err)
	}
	return out, nil
}

// ResolveSecret returns the value of a managed secret.
func (s *Store) ResolveSecret(ctx context.Context, name string) ([]byte, error) {
	var (
		kind       string
		kekID      sql.NullString
		wrapped    []byte
		nonce      []byte
		ciphertext []byte
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, kek_id, dek_wrapped, nonce, ciphertext FROM secrets WHERE name = $1`,
		name).Scan(&kind, &kekID, &wrapped, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read secret %s: %w", name, err)
	}
	if kind != SecretManaged {
		return nil, ErrSecretNotManaged
	}
	if len(s.kek) == 0 {
		return nil, ErrNoKEK
	}
	if kekID.String != s.kekID {
		// A secret encrypted under a key this process does not hold. Saying so
		// is the difference between a five-minute fix and a hunt through a
		// decryption error.
		return nil, fmt.Errorf("store: secret %s is wrapped under key %q, this process holds %q",
			name, kekID.String, s.kekID)
	}

	if len(wrapped) < 12 {
		return nil, fmt.Errorf("store: secret %s has a malformed wrapped key", name)
	}
	dek, err := openAESGCM(s.kek, wrapped[:12], wrapped[12:])
	if err != nil {
		return nil, fmt.Errorf("store: unwrap data key for %s: %w", name, err)
	}
	value, err := openAESGCM(dek, nonce, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("store: decrypt secret %s: %w", name, err)
	}
	return value, nil
}

// ListSecrets returns the stored secrets, values excluded.
func (s *Store) ListSecrets(ctx context.Context) ([]Secret, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, kind, COALESCE(ref_uri, ''), COALESCE(kek_id, ''),
		       created_by, created_at, updated_at, rotated_at
		FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	defer rows.Close()

	var out []Secret
	for rows.Next() {
		var sec Secret
		if err := rows.Scan(&sec.ID, &sec.Name, &sec.Kind, &sec.RefURI, &sec.KEKID,
			&sec.CreatedBy, &sec.CreatedAt, &sec.UpdatedAt, nullTimeScanner(&sec.RotatedAt)); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		out = append(out, sec)
	}
	return out, rows.Err()
}

func sealAESGCM(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("store: generate nonce: %w", err)
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil), nil
}

func openAESGCM(key, nonce, ciphertext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("store: decrypt: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: build GCM: %w", err)
	}
	return gcm, nil
}

// nullTimeScanner scans a nullable timestamp straight into a *time.Time field.
type nullTimeDest struct{ dest **time.Time }

func nullTimeScanner(dest **time.Time) sql.Scanner { return nullTimeDest{dest: dest} }

func (d nullTimeDest) Scan(src any) error {
	var t sql.NullTime
	if err := t.Scan(src); err != nil {
		return err
	}
	*d.dest = timePtr(t)
	return nil
}
