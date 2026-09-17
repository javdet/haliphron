package db

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
)

//go:embed queries/*.sql
var queriesFS embed.FS

// Queries that are contract rather than implementation.
//
// The lease, the two expiry scanners and the row lock the ingest path takes are
// not ordinary data access: their exact shape is what sections 4 and 5 of the
// Cluster API contract describe, and getting one of them subtly wrong produces
// a system that works until two clusters poll at the same moment. They live
// here, beside the schema they depend on, so that the backend and the contract
// tests execute the same text — a test that re-types the lease statement is
// testing its own copy.
var (
	queriesOnce sync.Once
	queries     map[string]string
	queriesErr  error
)

// Query returns the named contract query, panicking if it does not exist. The
// name is the file's base name without the extension: "lease", "expire_ack",
// "expire_lease", "lock_run".
//
// It panics rather than returning an error because the set is fixed at compile
// time by go:embed: a missing name is a typo in a constant, not a runtime
// condition any caller could handle.
func Query(name string) string {
	loadQueries()
	if queriesErr != nil {
		panic(queriesErr)
	}
	q, ok := queries[name]
	if !ok {
		panic(fmt.Sprintf("db: no such query %q; have %s", name, strings.Join(QueryNames(), ", ")))
	}
	return q
}

// QueryNames lists the available contract queries, sorted.
func QueryNames() []string {
	loadQueries()
	names := make([]string, 0, len(queries))
	for n := range queries {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func loadQueries() {
	queriesOnce.Do(func() {
		entries, err := fs.ReadDir(queriesFS, "queries")
		if err != nil {
			queriesErr = fmt.Errorf("read embedded queries: %w", err)
			return
		}
		queries = make(map[string]string, len(entries))
		for _, e := range entries {
			b, err := queriesFS.ReadFile("queries/" + e.Name())
			if err != nil {
				queriesErr = fmt.Errorf("read embedded query %s: %w", e.Name(), err)
				return
			}
			queries[strings.TrimSuffix(e.Name(), ".sql")] = string(b)
		}
	})
}
