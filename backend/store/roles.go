package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
)

// Roles, stored whole.
//
// The spec is jsonb rather than columns because it is a product object whose
// shape changes with the product: phase 2 adds the repository resolution chain
// to it without touching this table. Admission reads the parts it understands
// and ignores the rest, which is the same compatibility rule the wire
// contracts follow.
//
// There is no foreign key from runs.role_name to this table, deliberately. A
// run records which role it was admitted under, and that record has to survive
// the role being renamed or deleted — the run's spec is frozen, and its role is
// part of what it froze.

// Role is a row.
type Role struct {
	ID        runv1.ULID
	Name      string
	Spec      json.RawMessage
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertRole creates a role or replaces its spec, returning the stored row.
func (s *Store) UpsertRole(ctx context.Context, name string, spec json.RawMessage, by string) (Role, error) {
	var r Role
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO roles (id, name, spec, created_by)
		VALUES ($1, $2, $3::jsonb, $4)
		ON CONFLICT (tenant_id, name) WHERE deleted_at IS NULL
		DO UPDATE SET spec = EXCLUDED.spec
		RETURNING id, name, spec, created_by, created_at, updated_at`,
		newID(), name, []byte(spec), by).Scan(
		&r.ID, &r.Name, &r.Spec, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return Role{}, fmt.Errorf("store: upsert role %s: %w", name, err)
	}
	return r, nil
}

// RoleByName reads a live role.
func (s *Store) RoleByName(ctx context.Context, name string) (Role, error) {
	var r Role
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, spec, created_by, created_at, updated_at
		FROM roles WHERE name = $1 AND deleted_at IS NULL`, name).Scan(
		&r.ID, &r.Name, &r.Spec, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Role{}, ErrNotFound
	}
	if err != nil {
		return Role{}, fmt.Errorf("store: read role %s: %w", name, err)
	}
	return r, nil
}

// ListRoles returns the live roles, by name.
func (s *Store) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, spec, created_by, created_at, updated_at
		FROM roles WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: list roles: %w", err)
	}
	defer rows.Close()

	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Spec, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan role: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRole is a soft delete. Runs admitted under the role keep naming it,
// and the partial unique index lets the name be used again.
func (s *Store) DeleteRole(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE roles SET deleted_at = now() WHERE name = $1 AND deleted_at IS NULL`, name)
	if err != nil {
		return fmt.Errorf("store: delete role %s: %w", name, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
