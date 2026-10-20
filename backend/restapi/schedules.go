package restapi

import (
	"encoding/json"
	"net/http"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
)

// The schedule endpoints: run_job as REST. A schedule is a run template, a cron
// expression and a timezone; what the fields mean is app's, and this file only
// translates them.

// scheduleRequest is a schedule as a create body.
type scheduleRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	Cron     string `json:"cron"`
	Timezone string `json:"timezone,omitempty"`
	// Enabled defaults to true: a schedule created switched off is one that
	// was asked for explicitly.
	Enabled *bool `json:"enabled,omitempty"`

	ConcurrencyPolicy       string `json:"concurrency_policy,omitempty"`
	StartingDeadlineSeconds *int32 `json:"starting_deadline_seconds,omitempty"`

	Run runFields `json:"run"`
}

// schedulePatch is a partial edit. An absent field is unchanged; run, when
// present, replaces the whole template; a starting_deadline_seconds of 0
// clears the deadline.
type schedulePatch struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`

	Cron     *string `json:"cron,omitempty"`
	Timezone *string `json:"timezone,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`

	ConcurrencyPolicy       *string `json:"concurrency_policy,omitempty"`
	StartingDeadlineSeconds *int32  `json:"starting_deadline_seconds,omitempty"`

	Run *runFields `json:"run,omitempty"`
}

// scheduleResponse is a schedule as the API shows it.
type scheduleResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Enabled  bool   `json:"enabled"`

	ConcurrencyPolicy       string `json:"concurrency_policy"`
	StartingDeadlineSeconds *int32 `json:"starting_deadline_seconds,omitempty"`

	Run runFields `json:"run"`

	NextRunAt   *time.Time `json:"next_run_at,omitempty"`
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
	LastRunID   string     `json:"last_run_id,omitempty"`
	LastOutcome string     `json:"last_outcome,omitempty"`
	LastError   string     `json:"last_error,omitempty"`

	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

func scheduleView(sch app.Schedule) scheduleResponse {
	return scheduleResponse{
		ID:                      string(sch.ID),
		Name:                    sch.Name,
		Description:             sch.Description,
		Cron:                    sch.Cron,
		Timezone:                sch.Timezone,
		Enabled:                 sch.Enabled,
		ConcurrencyPolicy:       sch.ConcurrencyPolicy,
		StartingDeadlineSeconds: sch.StartingDeadlineSeconds,
		Run:                     runFieldsOf(sch.Template),
		NextRunAt:               sch.NextRunAt,
		LastFiredAt:             sch.LastFiredAt,
		LastRunID:               string(sch.LastRunID),
		LastOutcome:             sch.LastOutcome,
		LastError:               sch.LastError,
		CreatedBy:               sch.CreatedBy,
		CreatedAt:               sch.CreatedAt,
		UpdatedBy:               sch.UpdatedBy,
		UpdatedAt:               sch.UpdatedAt,
	}
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, _ caller) {
	schedules, err := s.app.Schedules(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]scheduleResponse, 0, len(schedules))
	for _, sch := range schedules {
		items = append(items, scheduleView(sch))
	}
	s.write(w, http.StatusOK, map[string]any{"schedules": items})
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request, _ caller) {
	sch, err := s.app.Schedule(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, scheduleView(sch))
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, c caller) {
	var req scheduleRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	sch, err := s.app.CreateSchedule(r.Context(), app.ScheduleInput{
		Name:                    req.Name,
		Description:             req.Description,
		Cron:                    req.Cron,
		Timezone:                req.Timezone,
		Enabled:                 enabled,
		ConcurrencyPolicy:       req.ConcurrencyPolicy,
		StartingDeadlineSeconds: req.StartingDeadlineSeconds,
		Run:                     req.Run.submitRequest(),
	}, c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusCreated, scheduleView(sch))
}

func (s *Server) patchSchedule(w http.ResponseWriter, r *http.Request, c caller) {
	var req schedulePatch
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	patch := app.SchedulePatch{
		Name:                    req.Name,
		Description:             req.Description,
		Cron:                    req.Cron,
		Timezone:                req.Timezone,
		Enabled:                 req.Enabled,
		ConcurrencyPolicy:       req.ConcurrencyPolicy,
		StartingDeadlineSeconds: req.StartingDeadlineSeconds,
	}
	if req.Run != nil {
		template := req.Run.submitRequest()
		patch.Run = &template
	}
	sch, err := s.app.UpdateSchedule(r.Context(), runv1.ULID(r.PathValue("id")), patch, c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, scheduleView(sch))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.app.DeleteSchedule(r.Context(), runv1.ULID(r.PathValue("id")), c.Name(), c.Via()); err != nil {
		s.failFor(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// triggerSchedule is "run now": the template admitted at once, answered like
// POST /runs — 202 with the run, or the first answer again under a known
// Idempotency-Key.
func (s *Server) triggerSchedule(w http.ResponseWriter, r *http.Request, c caller) {
	var empty struct{}
	raw, ok := s.decode(w, r, &empty)
	if !ok {
		return
	}

	opts := app.SubmitOptions{Key: r.Header.Get("Idempotency-Key"), Body: raw, Scope: "schedules.run"}
	submitted, err := s.app.TriggerSchedule(r.Context(), runv1.ULID(r.PathValue("id")), c.Name(), c.Via(), opts)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	if submitted.Replayed {
		if len(submitted.Response) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(submitted.Response)
			return
		}
		s.write(w, http.StatusOK, RunView(submitted.Run))
		return
	}

	view := RunView(submitted.Run)
	body, err := json.Marshal(view)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if opts.Key != "" {
		if err := s.app.CompleteSubmission(r.Context(), opts, submitted.Run.ID, http.StatusAccepted, body); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	s.write(w, http.StatusAccepted, view)
}
