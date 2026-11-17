package restapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/store"
)

// The alert receiver endpoints: an operator configures what a Prometheus
// Alertmanager notification starts, and Alertmanager delivers notifications.
// What the fields mean, which alerts a run carries and the throttle are app's;
// this file translates them.
//
// The delivery endpoint is the one route here outside the bearer-token
// scheme's scopes. Alertmanager presents the receiver's own credential as a
// bearer token, and app checks it against that receiver alone.

// alertReceiverRequest is a receiver as a create body.
type alertReceiverRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Enabled defaults to true.
	Enabled *bool `json:"enabled,omitempty"`
	// ThrottleSeconds defaults to an hour; 0 turns suppression off.
	ThrottleSeconds *int32 `json:"throttle_seconds,omitempty"`

	Run runFields `json:"run"`

	// ChatReply is the team-chat channel the runs answer in, in the thread of
	// the post that announced the alerts.
	ChatReply *chatReplyRequest `json:"chat_reply,omitempty"`
}

// chatReplyRequest names a receiver's channel. Its name is the platform's,
// read when it is saved.
type chatReplyRequest struct {
	Platform  string `json:"platform"`
	ChannelID string `json:"channel_id"`
}

func (c *chatReplyRequest) app() *app.AlertChatReply {
	if c == nil {
		return nil
	}
	return &app.AlertChatReply{Platform: c.Platform, ChannelID: c.ChannelID}
}

// chatReplyResponse is a receiver's channel as the API shows it.
type chatReplyResponse struct {
	Platform    string `json:"platform"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

// alertReceiverPatch is a partial edit; run, when present, replaces the whole
// template.
type alertReceiverPatch struct {
	Name            *string `json:"name,omitempty"`
	Description     *string `json:"description,omitempty"`
	Enabled         *bool   `json:"enabled,omitempty"`
	ThrottleSeconds *int32  `json:"throttle_seconds,omitempty"`

	Run *runFields `json:"run,omitempty"`

	// ChatReply is absent to leave the channel alone, null to clear it, or
	// the new channel. Raw, because absent and null mean different things.
	ChatReply json.RawMessage `json:"chat_reply,omitempty"`
}

// alertReceiverResponse is a receiver as the API shows it, without its
// credential.
type alertReceiverResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`

	Run             runFields `json:"run"`
	ThrottleSeconds int32     `json:"throttle_seconds"`

	// ChatReply is the team-chat channel the runs answer in, null for none.
	// ChatReplyPlatformEnabled is false when one is set and its platform's
	// integration is switched off: the runs then owe no reply.
	ChatReply                *chatReplyResponse `json:"chat_reply"`
	ChatReplyPlatformEnabled bool               `json:"chat_reply_platform_enabled"`

	// WebhookPath is where Alertmanager delivers, relative to the API's
	// origin.
	WebhookPath string `json:"webhook_path"`

	LastReceivedAt *time.Time `json:"last_received_at,omitempty"`
	LastOutcome    string     `json:"last_outcome,omitempty"`
	LastRunID      string     `json:"last_run_id,omitempty"`
	LastError      string     `json:"last_error,omitempty"`

	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// alertReceiverCredential is a receiver with its credential, which create and
// rotate-token answer with and nothing else ever does.
type alertReceiverCredential struct {
	alertReceiverResponse
	Token string `json:"token"`
}

type alertDeliveryResponse struct {
	ID         string    `json:"id"`
	ReceivedAt time.Time `json:"received_at"`
	GroupKey   string    `json:"group_key"`
	Status     string    `json:"status"`
	Firing     int32     `json:"firing"`
	Resolved   int32     `json:"resolved"`
	Throttled  []string  `json:"throttled"`
	Outcome    string    `json:"outcome"`
	RunID      string    `json:"run_id,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type alertSuppressionResponse struct {
	Fingerprint     string    `json:"fingerprint"`
	Alertname       string    `json:"alertname"`
	SuppressedUntil time.Time `json:"suppressed_until"`
	RunID           string    `json:"run_id,omitempty"`
	FirstRunAt      time.Time `json:"first_run_at"`
	LastRunAt       time.Time `json:"last_run_at"`
	Runs            int32     `json:"runs"`
	Suppressed      int64     `json:"suppressed"`
}

// alertReceiverViews renders receivers, reading each chat platform's state
// once.
func (s *Server) alertReceiverViews(ctx context.Context, receivers ...app.AlertReceiver) ([]alertReceiverResponse, error) {
	enabled := map[string]bool{}
	out := make([]alertReceiverResponse, 0, len(receivers))
	for _, rc := range receivers {
		v := alertReceiverView(rc)
		if rc.ChatPlatform != "" {
			on, seen := enabled[rc.ChatPlatform]
			if !seen {
				var err error
				if on, err = s.app.ChatIntegrationEnabled(ctx, rc.ChatPlatform); err != nil {
					return nil, err
				}
				enabled[rc.ChatPlatform] = on
			}
			v.ChatReply = &chatReplyResponse{
				Platform: rc.ChatPlatform, ChannelID: rc.ChatChannelID, ChannelName: rc.ChatChannelName,
			}
			v.ChatReplyPlatformEnabled = on
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) alertReceiverOne(ctx context.Context, rc app.AlertReceiver) (alertReceiverResponse, error) {
	views, err := s.alertReceiverViews(ctx, rc)
	if err != nil {
		return alertReceiverResponse{}, err
	}
	return views[0], nil
}

func alertReceiverView(r app.AlertReceiver) alertReceiverResponse {
	return alertReceiverResponse{
		ID:              string(r.ID),
		Name:            r.Name,
		Description:     r.Description,
		Enabled:         r.Enabled,
		Run:             runFieldsOf(r.Template),
		ThrottleSeconds: r.ThrottleSeconds,
		WebhookPath:     BasePath + "/alert-receivers/" + string(r.ID) + "/alerts",
		LastReceivedAt:  r.LastReceivedAt,
		LastOutcome:     r.LastOutcome,
		LastRunID:       string(r.LastRunID),
		LastError:       r.LastError,
		CreatedBy:       r.CreatedBy,
		CreatedAt:       r.CreatedAt,
		UpdatedBy:       r.UpdatedBy,
		UpdatedAt:       r.UpdatedAt,
	}
}

func (s *Server) listAlertReceivers(w http.ResponseWriter, r *http.Request, _ caller) {
	receivers, err := s.app.AlertReceivers(r.Context())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items, err := s.alertReceiverViews(r.Context(), receivers...)
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, map[string]any{"alert_receivers": items})
}

func (s *Server) getAlertReceiver(w http.ResponseWriter, r *http.Request, _ caller) {
	rc, err := s.app.AlertReceiver(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeAlertReceiver(w, r, http.StatusOK, rc, "")
}

func (s *Server) createAlertReceiver(w http.ResponseWriter, r *http.Request, c caller) {
	var req alertReceiverRequest
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rc, credential, err := s.app.CreateAlertReceiver(r.Context(), app.AlertReceiverInput{
		Name:            req.Name,
		Description:     req.Description,
		Enabled:         enabled,
		ThrottleSeconds: req.ThrottleSeconds,
		Run:             req.Run.submitRequest(),
		ChatReply:       req.ChatReply.app(),
	}, c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeAlertReceiver(w, r, http.StatusCreated, rc, credential)
}

func (s *Server) patchAlertReceiver(w http.ResponseWriter, r *http.Request, c caller) {
	var req alertReceiverPatch
	if _, ok := s.decode(w, r, &req); !ok {
		return
	}
	patch := app.AlertReceiverPatch{
		Name:            req.Name,
		Description:     req.Description,
		Enabled:         req.Enabled,
		ThrottleSeconds: req.ThrottleSeconds,
	}
	if req.Run != nil {
		template := req.Run.submitRequest()
		patch.Run = &template
	}
	if req.ChatReply != nil {
		patch.ChatReplySet = true
		if !bytes.Equal(bytes.TrimSpace(req.ChatReply), []byte("null")) {
			var reply chatReplyRequest
			if err := json.Unmarshal(req.ChatReply, &reply); err != nil {
				s.fail(w, http.StatusBadRequest, "invalid_request",
					"chat_reply is an object with platform and channel_id, or null", "chat_reply")
				return
			}
			patch.ChatReply = reply.app()
		}
	}
	rc, err := s.app.UpdateAlertReceiver(r.Context(), runv1.ULID(r.PathValue("id")), patch, c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.writeAlertReceiver(w, r, http.StatusOK, rc, "")
}

func (s *Server) deleteAlertReceiver(w http.ResponseWriter, r *http.Request, c caller) {
	if err := s.app.DeleteAlertReceiver(r.Context(), runv1.ULID(r.PathValue("id")), c.Name(), c.Via()); err != nil {
		s.failFor(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateAlertReceiverToken(w http.ResponseWriter, r *http.Request, c caller) {
	rc, credential, err := s.app.RotateAlertReceiverToken(r.Context(), runv1.ULID(r.PathValue("id")), c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeAlertReceiver(w, r, http.StatusOK, rc, credential)
}

// writeAlertReceiver answers with a receiver, and with its credential when one
// was just minted. The change is made by now, so a failure to read a chat
// platform's state shows the receiver without it rather than failing.
func (s *Server) writeAlertReceiver(w http.ResponseWriter, r *http.Request, status int,
	rc app.AlertReceiver, credential string) {

	view, err := s.alertReceiverOne(r.Context(), rc)
	if err != nil {
		s.log.Warn("could not read whether an alert receiver's chat platform is enabled",
			"receiver", rc.ID, "error", err)
		view = alertReceiverView(rc)
		if rc.ChatPlatform != "" {
			view.ChatReply = &chatReplyResponse{
				Platform: rc.ChatPlatform, ChannelID: rc.ChatChannelID, ChannelName: rc.ChatChannelName,
			}
		}
	}
	if credential != "" {
		s.write(w, status, alertReceiverCredential{view, credential})
		return
	}
	s.write(w, status, view)
}

func (s *Server) listAlertDeliveries(w http.ResponseWriter, r *http.Request, _ caller) {
	deliveries, err := s.app.AlertDeliveries(r.Context(), runv1.ULID(r.PathValue("id")),
		runv1.ULID(r.URL.Query().Get("before")), intParam(r, "limit", 50))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]alertDeliveryResponse, 0, len(deliveries))
	for _, d := range deliveries {
		throttled := d.Throttled
		if throttled == nil {
			throttled = []string{}
		}
		items = append(items, alertDeliveryResponse{
			ID: string(d.ID), ReceivedAt: d.ReceivedAt, GroupKey: d.GroupKey, Status: d.Status,
			Firing: d.Firing, Resolved: d.Resolved, Throttled: throttled, Outcome: d.Outcome,
			RunID: string(d.RunID), Error: d.Error,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"deliveries": items})
}

func (s *Server) listAlertSuppressions(w http.ResponseWriter, r *http.Request, _ caller) {
	suppressions, err := s.app.AlertSuppressions(r.Context(), runv1.ULID(r.PathValue("id")))
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	items := make([]alertSuppressionResponse, 0, len(suppressions))
	for _, a := range suppressions {
		items = append(items, alertSuppressionResponse{
			Fingerprint: a.Fingerprint, Alertname: a.Alertname, SuppressedUntil: a.SuppressedUntil,
			RunID: string(a.RunID), FirstRunAt: a.FirstRunAt, LastRunAt: a.LastRunAt,
			Runs: a.Runs, Suppressed: a.Suppressed,
		})
	}
	s.write(w, http.StatusOK, map[string]any{"suppressions": items})
}

// clearAlertSuppressions lifts one suppression when the path names a
// fingerprint, and every live one when it does not.
func (s *Server) clearAlertSuppressions(w http.ResponseWriter, r *http.Request, c caller) {
	n, err := s.app.ClearAlertSuppressions(r.Context(), runv1.ULID(r.PathValue("id")),
		r.PathValue("fingerprint"), c.Name(), c.Via())
	if err != nil {
		s.failFor(w, r, err)
		return
	}
	s.write(w, http.StatusOK, map[string]any{"cleared": n})
}

// receiveAlerts is Alertmanager's webhook_config url.
//
// It answers synchronously: admission is a few statements, and Alertmanager
// waits for the answer to decide whether to retry. A 5xx is retried, so it is
// kept for the store failing; a notification admission refused is a 200 with
// the outcome, because retrying a configuration problem changes nothing.
func (s *Server) receiveAlerts(w http.ResponseWriter, r *http.Request) {
	credential, _ := bearer(r)

	body, err := io.ReadAll(io.LimitReader(r.Body, app.MaxAlertPayloadBytes+1))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_request", "the request body could not be read", "")
		return
	}

	outcome, err := s.app.ReceiveAlerts(r.Context(), runv1.ULID(r.PathValue("id")), credential, body)
	var payload *app.AlertPayloadError
	switch {
	case errors.Is(err, app.ErrAlertCredentialInvalid):
		// An unknown receiver is the same answer: without a valid credential
		// nothing is said about which receivers exist.
		s.fail(w, http.StatusUnauthorized, "unauthenticated", "the alert receiver credential is not valid", "")
		return
	case errors.Is(err, app.ErrAlertReceiverDisabled), errors.Is(err, store.ErrNotFound):
		s.fail(w, http.StatusNotFound, "not_found", "this alert receiver is disabled or gone", "")
		return
	case errors.As(err, &payload):
		s.fail(w, http.StatusBadRequest, "invalid_request", payload.Detail, "")
		return
	case err != nil:
		// Alertmanager retries a 5xx, and a retry of a notification whose
		// first attempt committed is absorbed by the throttle.
		s.log.Error("an alert notification could not be handled", "path", r.URL.Path, "error", err)
		w.Header().Set("Retry-After", "5")
		s.fail(w, http.StatusServiceUnavailable, "unavailable", "the notification could not be handled; retry", "")
		return
	}

	answer := map[string]any{"outcome": outcome.Outcome, "alerts": outcome.Alerts, "throttled": outcome.Throttled}
	if outcome.RunID != "" {
		answer["run_id"] = string(outcome.RunID)
	}
	if outcome.Error != "" {
		answer["error"] = outcome.Error
	}
	s.write(w, http.StatusOK, answer)
}
