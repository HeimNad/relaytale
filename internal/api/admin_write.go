package api

import (
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"relaytale/internal/provider"
	"relaytale/internal/queue"
	"relaytale/internal/suppression"
)

func validReason(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 2048 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func (a Management) createProvider(w http.ResponseWriter, r *http.Request) {
	var v struct {
		ID       string            `json:"id"`
		Settings provider.Settings `json:"settings"`
		Password string            `json:"password"`
		Reason   string            `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	rev, err := provider.Manage(r.Context(), a.DB, a.Box, "create", v.ID, 0, v.Settings, v.Password, actor(r), v.Reason)
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 201, map[string]any{"id": v.ID, "revision": rev})
}
func (a Management) configureProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v struct {
		Expected int64             `json:"expected_revision"`
		Settings provider.Settings `json:"settings"`
		Reason   string            `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	rev, err := provider.Manage(r.Context(), a.DB, a.Box, "configure", id, v.Expected, v.Settings, "", actor(r), v.Reason)
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]any{"id": id, "revision": rev})
}
func (a Management) rotateProvider(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v struct {
		Expected int64  `json:"expected_revision"`
		Password string `json:"password"`
		Reason   string `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	rev, err := provider.Manage(r.Context(), a.DB, a.Box, "credentials", id, v.Expected, provider.Settings{}, v.Password, actor(r), v.Reason)
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]any{"id": id, "revision": rev})
}
func (a Management) addSuppression(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Email     string     `json:"email"`
		Category  string     `json:"category"`
		Reason    string     `json:"reason"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !body(w, r, &v) {
		return
	}
	addr, err := mail.ParseAddress(v.Email)
	categories := map[string]bool{"manual": true, "hard_bounce": true, "complaint": true, "invalid": true, "unsubscribe": true}
	if err != nil || addr.Address != v.Email || len(v.Email) > 320 || strings.ContainsAny(v.Email, "\r\n\x00") || !utf8.ValidString(v.Email) || !categories[v.Category] || !validReason(v.Reason) || (v.ExpiresAt != nil && !v.ExpiresAt.After(time.Now())) {
		respond(w, 400, "invalid_request")
		return
	}
	id, err := (suppression.Service{DB: a.DB}).Add(r.Context(), suppression.AddRequest{Email: v.Email, Category: v.Category, Reason: v.Reason, ExpiresAt: v.ExpiresAt, Actor: actor(r)})
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 201, map[string]string{"id": id})
}
func (a Management) releaseSuppression(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v struct {
		Reason string `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	if !validReason(v.Reason) {
		respond(w, 400, "invalid_request")
		return
	}
	if err := (suppression.Service{DB: a.DB}).Release(r.Context(), id, actor(r), v.Reason); err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]string{"id": id, "status": "released"})
}
func (a Management) resolve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v struct {
		Expected    string `json:"expected_attempt"`
		Action      string `json:"action"`
		Reason      string `json:"reason"`
		Acknowledge bool   `json:"acknowledge_duplicate_risk"`
	}
	if !body(w, r, &v) {
		return
	}
	_, err := uuid.Parse(v.Expected)
	if err != nil || !validReason(v.Reason) || (v.Action != "retry" && v.Action != "mark-delivered" && v.Action != "mark-failed") || (v.Action == "retry" && !v.Acknowledge) {
		respond(w, 400, "invalid_request")
		return
	}
	audit, err := (queue.Repository{DB: a.DB}).ResolveUnknown(r.Context(), queue.Resolution{RecipientID: id, ExpectedAttempt: v.Expected, Action: v.Action, Reason: v.Reason, Actor: actor(r), AcknowledgeDuplicate: v.Acknowledge})
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]string{"audit_id": audit})
}
