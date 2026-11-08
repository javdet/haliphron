package restapi

import (
	"net/http"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// The lock and deferred-run endpoints: what agents hold and what they are
// waiting for, as an operator sees it. Setting a lock and deferring work are
// an agent's verbs and live in MCP; what is here is reading, the operator's
// force-release, and cancelling a waiting row.

type lockResponse struct {
	LockID      string    `json:"lock_id"`
	Seq         int64     `json:"seq"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	AcquiredAt  time.Time `json:"acquired_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ExpiresAt   time.Time `json:"expires_at"`

	HeldByRun            string     `json:"held_by_run"`
	HolderStatus         string     `json:"holder_status"`
	HolderRole           string     `json:"holder_role,omitempty"`
	HolderCreatedAt      time.Time  `json:"holder_created_at"`
	HolderStartedAt      *time.Time `json:"holder_started_at,omitempty"`
	HolderLastActivityAt time.Time  `json:"holder_last_activity_at"`
}

func lockView(l store.Lock) lockResponse {
	return lockResponse{
		LockID:               string(l.ID),
		Seq:                  l.Seq,
		Name:                 l.Name,
		Description:          l.Description,
		AcquiredAt:           l.AcquiredAt,
		UpdatedAt:            l.UpdatedAt,
		ExpiresAt:            l.ExpiresAt,
		HeldByRun:            string(l.RunID),
		HolderStatus:         l.HolderStatus,
		HolderRole:           l.HolderRole,
		HolderCreatedAt:      l.HolderCreatedAt,
		HolderStartedAt:      l.HolderStartedAt,
		HolderLastActivityAt: l.HolderLastActivityAt,
	}
}

func (s *Server) listLocks(w http.ResponseWriter, r *http.Request, _ caller) {
	locks, err := s.app.Locks(r.Context(), store.LockFilter{RunID: runv1.ULID(r.URL.Query().Get("run_id"))})
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]lockResponse, 0, len(locks))
	for _, l := range locks {
		items = append(items, lockView(l))
	}
	s.write(w, http.StatusOK, map[string]any{"locks": items})
}

func (s *Server) deleteLock(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.app.ForceUnsetLock(r.Context(), runv1.ULID(r.PathValue("id")), c.Name(), c.Via()); err != nil {
		s.failFor(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deferredLock struct {
	LockID string `json:"lock_id"`
	// Live is whether the lock is still held. The deferred run starts when
	// none of its locks is.
	Live bool `json:"live"`
}

type deferredResponse struct {
	DeferredID   string         `json:"deferred_id"`
	ParentRunID  string         `json:"parent_run_id"`
	State        string         `json:"state"`
	Run          runFields      `json:"run"`
	WaitForLocks []deferredLock `json:"wait_for_locks"`
	NotBefore    *time.Time     `json:"not_before,omitempty"`
	GiveUpAt     time.Time      `json:"give_up_at"`
	RunID        string         `json:"run_id,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	CreatedBy    string         `json:"created_by"`
	CreatedAt    time.Time      `json:"created_at"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
}

func deferredView(d app.DeferredRun) deferredResponse {
	live := make(map[runv1.ULID]bool, len(d.LiveLocks))
	for _, id := range d.LiveLocks {
		live[id] = true
	}
	locks := make([]deferredLock, 0, len(d.WaitForLocks))
	for _, id := range d.WaitForLocks {
		locks = append(locks, deferredLock{LockID: string(id), Live: live[id]})
	}
	return deferredResponse{
		DeferredID:   string(d.ID),
		ParentRunID:  string(d.ParentRunID),
		State:        d.State,
		Run:          runFieldsOf(d.Template),
		WaitForLocks: locks,
		NotBefore:    d.NotBefore,
		GiveUpAt:     d.GiveUpAt,
		RunID:        string(d.RunID),
		Reason:       d.Reason,
		CreatedBy:    d.CreatedBy,
		CreatedAt:    d.CreatedAt,
		FinishedAt:   d.FinishedAt,
	}
}

func (s *Server) listDeferred(w http.ResponseWriter, r *http.Request, _ caller) {
	q := r.URL.Query()
	rows, err := s.app.DeferredRuns(r.Context(), store.DeferredFilter{
		State:  q.Get("state"),
		Parent: runv1.ULID(q.Get("parent_run_id")),
		RunID:  runv1.ULID(q.Get("run_id")),
		Limit:  intParam(r, "limit", 100),
	})
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]deferredResponse, 0, len(rows))
	for _, d := range rows {
		items = append(items, deferredView(d))
	}
	s.write(w, http.StatusOK, map[string]any{"deferred_runs": items})
}

func (s *Server) getDeferred(w http.ResponseWriter, r *http.Request, _ caller) {
	d, err := s.app.DeferredRun(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, deferredView(d))
}

func (s *Server) cancelDeferred(w http.ResponseWriter, r *http.Request, c caller) {
	d, err := s.app.CancelDeferred(r.Context(), runv1.ULID(r.PathValue("id")), c.Parent, c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, deferredView(d))
}
