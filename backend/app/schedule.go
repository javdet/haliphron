package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
	"github.com/robfig/cron/v3"
)

// Schedules: a run template admitted on a cron expression.
//
// Every rule about what a schedule may be is here, for the reason the package
// comment gives: the UI and REST must not save schedules by different rules,
// and when MCP grows a run_job tool it must not either. The rule that matters
// most is that a schedule is checked against admission itself when it is
// saved. A schedule whose role was mistyped is found out at the keyboard, not
// at three in the morning by a fire nobody is watching.
//
// When a schedule fires, and what a fire does, is scheduler.go.

// Concurrency policies, with batch/v1 CronJob's names and meanings.
const (
	// ConcurrencyAllow admits every fire, however many earlier runs are live.
	ConcurrencyAllow = "Allow"
	// ConcurrencyForbid skips a fire while an earlier run of the schedule is
	// live. The default, because an agent run costs money and an overlapping
	// pair of them on one repository is rarely what anyone meant.
	ConcurrencyForbid = "Forbid"
	// ConcurrencyReplace admits the fire and cancels the earlier live runs.
	ConcurrencyReplace = "Replace"
)

// MaxScheduleDescriptionBytes bounds a schedule's description.
const MaxScheduleDescriptionBytes = 1024

var scheduleNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)

// ScheduleInput is a schedule as a caller defines it.
type ScheduleInput struct {
	Name        string
	Description string

	Cron     string
	Timezone string
	Enabled  bool

	ConcurrencyPolicy       string
	StartingDeadlineSeconds *int32

	// Run is the template every fire submits. CreatedBy, CreatedVia and the
	// parent fields are ignored: a fire is attributed to its schedule.
	Run run.SubmitRequest
}

// SchedulePatch is a partial edit. A nil field is left as it is. Run, when
// set, replaces the whole template, and a StartingDeadlineSeconds of zero
// clears the deadline.
type SchedulePatch struct {
	Name        *string
	Description *string

	Cron     *string
	Timezone *string
	Enabled  *bool

	ConcurrencyPolicy       *string
	StartingDeadlineSeconds *int32

	Run *run.SubmitRequest
}

// Schedule is a stored schedule with its template decoded.
type Schedule struct {
	store.Schedule
	Template run.SubmitRequest
}

// ErrScheduleFromAgent is a schedule written with a per-run token.
//
// An agent pod holds runs:write so that it can start children it waits for.
// A schedule is different in kind — it outlives the run, spends on its own
// for as long as nobody notices, and is exactly what a prompt injection would
// ask for — so it is an operator's object, and the token of a run is refused.
var ErrScheduleFromAgent = errors.New("app: a run's own token cannot manage schedules")

// CronSchedule is a parsed cron expression in its timezone.
type CronSchedule struct {
	expr cron.Schedule
	loc  *time.Location
}

// Next is the first fire strictly after the given time, or the zero time if
// the expression never fires again.
func (c CronSchedule) Next(after time.Time) time.Time {
	next := c.expr.Next(after.In(c.loc))
	if next.IsZero() {
		return next
	}
	// UTC and whole seconds: the value is compared for equality by the fence,
	// and what is compared has to survive a round trip through timestamptz.
	return next.UTC().Truncate(time.Second)
}

var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseCron parses a five-field cron expression, or one of the @hourly-style
// descriptors, in an IANA timezone. An empty timezone is UTC.
func ParseCron(expr, timezone string) (CronSchedule, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return CronSchedule{}, &run.InvalidRequestError{Field: "cron", Detail: "a cron expression is required"}
	}
	if strings.HasPrefix(expr, "CRON_TZ=") || strings.HasPrefix(expr, "TZ=") {
		return CronSchedule{}, &run.InvalidRequestError{Field: "cron",
			Detail: "set the timezone in its own field rather than inside the expression"}
	}
	if strings.HasPrefix(expr, "@every") {
		// An interval rather than a calendar, with no timezone to mean
		// anything in and no minimum: "@every 1s" is a run per scheduler tick.
		return CronSchedule{}, &run.InvalidRequestError{Field: "cron",
			Detail: "@every is not supported; use a cron expression such as \"*/15 * * * *\""}
	}
	if timezone == "" {
		timezone = "UTC"
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil || timezone == "Local" {
		return CronSchedule{}, &run.InvalidRequestError{Field: "timezone",
			Detail: fmt.Sprintf("%q is not an IANA timezone such as Europe/Berlin", timezone)}
	}
	parsed, err := cronParser.Parse(expr)
	if err != nil {
		return CronSchedule{}, &run.InvalidRequestError{Field: "cron", Detail: err.Error()}
	}
	return CronSchedule{expr: parsed, loc: loc}, nil
}

// Schedules lists every schedule.
func (s *Service) Schedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.store.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Schedule, 0, len(rows))
	for _, row := range rows {
		sch, err := decodeSchedule(row)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, nil
}

// Schedule reads one schedule.
func (s *Service) Schedule(ctx context.Context, id runv1.ULID) (Schedule, error) {
	row, err := s.store.ScheduleByID(ctx, id)
	if err != nil {
		return Schedule{}, err
	}
	return decodeSchedule(row)
}

// CreateSchedule validates and stores a new schedule.
func (s *Service) CreateSchedule(ctx context.Context, in ScheduleInput, by, via string) (Schedule, error) {
	if via == "agent" {
		return Schedule{}, ErrScheduleFromAgent
	}
	in = normalizeSchedule(in)
	parsed, template, err := s.validateSchedule(ctx, in)
	if err != nil {
		return Schedule{}, err
	}

	now := s.now()
	id, err := run.NewULID(now)
	if err != nil {
		return Schedule{}, err
	}
	row, err := s.store.InsertSchedule(ctx, store.Schedule{
		ID:                      id,
		Name:                    in.Name,
		Description:             in.Description,
		Cron:                    in.Cron,
		Timezone:                in.Timezone,
		Enabled:                 in.Enabled,
		ConcurrencyPolicy:       in.ConcurrencyPolicy,
		StartingDeadlineSeconds: in.StartingDeadlineSeconds,
		Run:                     template,
		NextRunAt:               nextRunAt(in.Enabled, parsed, now),
		CreatedBy:               by,
	})
	if err != nil {
		return Schedule{}, err
	}
	s.auditSchedule(ctx, by, via, store.AuditScheduleCreated, row)
	return decodeSchedule(row)
}

// UpdateSchedule applies a partial edit.
//
// The next fire is kept when nothing that decides it changed, and recomputed
// from now when anything did — the expression, the timezone, or enabling a
// schedule that was off. Keeping it matters for a schedule that is due and has
// not fired yet: an edit to its description must not make it skip a slot.
func (s *Service) UpdateSchedule(ctx context.Context, id runv1.ULID, patch SchedulePatch, by, via string) (Schedule, error) {
	if via == "agent" {
		return Schedule{}, ErrScheduleFromAgent
	}
	current, err := s.Schedule(ctx, id)
	if err != nil {
		return Schedule{}, err
	}

	in := ScheduleInput{
		Name:                    current.Name,
		Description:             current.Description,
		Cron:                    current.Cron,
		Timezone:                current.Timezone,
		Enabled:                 current.Enabled,
		ConcurrencyPolicy:       current.ConcurrencyPolicy,
		StartingDeadlineSeconds: current.StartingDeadlineSeconds,
		Run:                     current.Template,
	}
	if patch.Name != nil {
		in.Name = *patch.Name
	}
	if patch.Description != nil {
		in.Description = *patch.Description
	}
	if patch.Cron != nil {
		in.Cron = *patch.Cron
	}
	if patch.Timezone != nil {
		in.Timezone = *patch.Timezone
	}
	if patch.Enabled != nil {
		in.Enabled = *patch.Enabled
	}
	if patch.ConcurrencyPolicy != nil {
		in.ConcurrencyPolicy = *patch.ConcurrencyPolicy
	}
	if patch.StartingDeadlineSeconds != nil {
		in.StartingDeadlineSeconds = patch.StartingDeadlineSeconds
		if *patch.StartingDeadlineSeconds == 0 {
			in.StartingDeadlineSeconds = nil
		}
	}
	if patch.Run != nil {
		in.Run = *patch.Run
	}

	in = normalizeSchedule(in)
	parsed, template, err := s.validateSchedule(ctx, in)
	if err != nil {
		return Schedule{}, err
	}

	next := current.NextRunAt
	if !in.Enabled || current.NextRunAt == nil ||
		in.Cron != current.Cron || in.Timezone != current.Timezone {
		next = nextRunAt(in.Enabled, parsed, s.now())
	}

	row, err := s.store.UpdateSchedule(ctx, store.Schedule{
		ID:                      id,
		Name:                    in.Name,
		Description:             in.Description,
		Cron:                    in.Cron,
		Timezone:                in.Timezone,
		Enabled:                 in.Enabled,
		ConcurrencyPolicy:       in.ConcurrencyPolicy,
		StartingDeadlineSeconds: in.StartingDeadlineSeconds,
		Run:                     template,
		NextRunAt:               next,
		UpdatedBy:               by,
	})
	if err != nil {
		return Schedule{}, err
	}
	s.auditSchedule(ctx, by, via, store.AuditScheduleUpdated, row)
	return decodeSchedule(row)
}

// DeleteSchedule removes a schedule. The runs it started are kept.
func (s *Service) DeleteSchedule(ctx context.Context, id runv1.ULID, by, via string) error {
	if via == "agent" {
		return ErrScheduleFromAgent
	}
	row, err := s.store.ScheduleByID(ctx, id)
	if err != nil {
		return err
	}
	if err := s.store.DeleteSchedule(ctx, id); err != nil {
		return err
	}
	s.auditSchedule(ctx, by, via, store.AuditScheduleDeleted, row)
	return nil
}

// TriggerSchedule admits the schedule's template now — "run now".
//
// It is an admission like any other, attributed to the caller rather than to
// the schedule, and linked to the schedule so its runs list together. It
// ignores the concurrency policy and leaves the next fire where it was, which
// is what creating a Job from a CronJob by hand does: the operator asked for a
// run, and a policy that refused them would be a policy they work around.
//
// The idempotency claim is taken over the schedule's identifier as well as the
// body, so one key sent to two schedules is a conflict rather than a replay of
// the first schedule's run.
func (s *Service) TriggerSchedule(ctx context.Context, id runv1.ULID, by, via string, opts SubmitOptions) (Submitted, error) {
	if via == "agent" {
		// The template's role and budget are the operator's, not the
		// calling run's; an agent wanting a run starts one of its own.
		return Submitted{}, ErrScheduleFromAgent
	}
	sch, err := s.Schedule(ctx, id)
	if err != nil {
		return Submitted{}, err
	}

	req := sch.Template
	req.CreatedBy = by
	req.CreatedVia = via
	if opts.Scope == "" {
		opts.Scope = "schedules.run"
	}
	opts.Body = append([]byte(string(id)+"\n"), opts.Body...)

	submitted, err := s.admit(ctx, req, opts, submitLinks{schedule: id})
	if err != nil {
		return Submitted{}, err
	}
	if !submitted.Replayed {
		if err := s.store.Audit(ctx, store.AuditEntry{
			Actor: by, ActorKind: actorKindFor(via), Action: store.AuditScheduleTriggered,
			SubjectKind: "schedule", SubjectID: string(id), RunID: submitted.Run.ID,
			Payload: map[string]any{"name": sch.Name},
		}); err != nil {
			return Submitted{}, err
		}
	}
	return submitted, nil
}

// validateSchedule checks a schedule and returns its parsed expression and the
// template as stored.
func (s *Service) validateSchedule(ctx context.Context, in ScheduleInput) (CronSchedule, json.RawMessage, error) {
	if !scheduleNamePattern.MatchString(in.Name) || len(in.Name) > 253 {
		return CronSchedule{}, nil, &run.InvalidRequestError{Field: "name",
			Detail: "a name is lower-case letters, digits, '-', '_' and '.', starting and ending with a letter or digit"}
	}
	if n := len(in.Description); n > MaxScheduleDescriptionBytes {
		return CronSchedule{}, nil, &run.InvalidRequestError{Field: "description",
			Detail: fmt.Sprintf("%d bytes is over the %d-byte limit", n, MaxScheduleDescriptionBytes)}
	}
	switch in.ConcurrencyPolicy {
	case ConcurrencyAllow, ConcurrencyForbid, ConcurrencyReplace:
	default:
		return CronSchedule{}, nil, &run.InvalidRequestError{Field: "concurrency_policy",
			Detail: fmt.Sprintf("%q is not one of Allow, Forbid, Replace", in.ConcurrencyPolicy)}
	}
	if d := in.StartingDeadlineSeconds; d != nil && *d <= 0 {
		return CronSchedule{}, nil, &run.InvalidRequestError{Field: "starting_deadline_seconds",
			Detail: "a starting deadline is a positive number of seconds"}
	}

	parsed, err := ParseCron(in.Cron, in.Timezone)
	if err != nil {
		return CronSchedule{}, nil, err
	}
	if parsed.Next(s.now()).IsZero() {
		return CronSchedule{}, nil, &run.InvalidRequestError{Field: "cron",
			Detail: "the expression never fires"}
	}

	// Admission's own rules, run now rather than at the first fire. The run
	// identifier prepare mints is thrown away; nothing is written.
	if _, _, err := s.prepare(ctx, in.Run); err != nil {
		var invalid *run.InvalidRequestError
		if errors.As(err, &invalid) {
			return CronSchedule{}, nil, &run.InvalidRequestError{
				Field: "run." + invalid.Field, Detail: invalid.Detail}
		}
		return CronSchedule{}, nil, err
	}

	template, err := json.Marshal(templateOf(in.Run))
	if err != nil {
		return CronSchedule{}, nil, fmt.Errorf("encode schedule template: %w", err)
	}
	return parsed, template, nil
}

func normalizeSchedule(in ScheduleInput) ScheduleInput {
	in.Name = strings.TrimSpace(in.Name)
	in.Cron = strings.TrimSpace(in.Cron)
	in.Timezone = strings.TrimSpace(in.Timezone)
	if in.Timezone == "" {
		in.Timezone = "UTC"
	}
	if in.ConcurrencyPolicy == "" {
		in.ConcurrencyPolicy = ConcurrencyForbid
	}
	// What a fire is attributed to is the scheduler's business, never the
	// template's: a template carrying a parent would be a child run of a run
	// that ended long ago.
	in.Run.CreatedBy, in.Run.CreatedVia = "", ""
	in.Run.ParentRunID, in.Run.Depth = "", 0
	return in
}

func nextRunAt(enabled bool, c CronSchedule, now time.Time) *time.Time {
	if !enabled {
		return nil
	}
	next := c.Next(now)
	return &next
}

func (s *Service) auditSchedule(ctx context.Context, by, via, action string, row store.Schedule) {
	if err := s.store.Audit(ctx, store.AuditEntry{
		Actor: by, ActorKind: actorKindFor(via), Action: action,
		SubjectKind: "schedule", SubjectID: string(row.ID),
		Payload: map[string]any{
			"name": row.Name, "cron": row.Cron, "timezone": row.Timezone,
			"enabled": row.Enabled, "concurrency_policy": row.ConcurrencyPolicy,
		},
	}); err != nil {
		// The schedule is saved; failing the request now would invite a retry
		// that saves it twice.
		s.log.Error("could not audit a schedule change", "schedule", row.ID, "action", action, "error", err)
	}
}

// scheduleTemplate is the template as stored: the public API's run request in
// its snake_case shape, so the row reads the same as the request that wrote it.
type scheduleTemplate struct {
	Prompt string `json:"prompt"`

	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	Role  string `json:"role,omitempty"`

	Repo         string `json:"repo,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	CreatePR     *bool  `json:"create_pr,omitempty"`

	TimeoutSeconds int32  `json:"timeout_seconds,omitempty"`
	MaxCostUSD     string `json:"max_cost_usd,omitempty"`
	MaxTurns       int32  `json:"max_turns,omitempty"`
	Priority       int32  `json:"priority,omitempty"`
}

func templateOf(req run.SubmitRequest) scheduleTemplate {
	return scheduleTemplate{
		Prompt:         req.Prompt,
		Agent:          string(req.Agent),
		Model:          req.Model,
		Role:           req.Role,
		Repo:           req.RepoURL,
		BaseBranch:     req.BaseBranch,
		TargetBranch:   req.TargetBranch,
		CreatePR:       req.CreatePR,
		TimeoutSeconds: req.TimeoutSeconds,
		MaxCostUSD:     string(req.MaxCostUSD),
		MaxTurns:       req.MaxTurns,
		Priority:       req.Priority,
	}
}

func decodeSchedule(row store.Schedule) (Schedule, error) {
	var t scheduleTemplate
	if err := json.Unmarshal(row.Run, &t); err != nil {
		return Schedule{}, fmt.Errorf("decode the template of schedule %s: %w", row.ID, err)
	}
	return Schedule{Schedule: row, Template: run.SubmitRequest{
		Prompt:         t.Prompt,
		Agent:          runv1.AgentType(t.Agent),
		Model:          t.Model,
		Role:           t.Role,
		RepoURL:        t.Repo,
		BaseBranch:     t.BaseBranch,
		TargetBranch:   t.TargetBranch,
		CreatePR:       t.CreatePR,
		TimeoutSeconds: t.TimeoutSeconds,
		MaxCostUSD:     runv1.MoneyUSD(t.MaxCostUSD),
		MaxTurns:       t.MaxTurns,
		Priority:       t.Priority,
	}}, nil
}
