package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

func (a Management) templateIdentities(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT k.id,jsonb_build_object('id',k.id,'name',k.name,'enabled',k.enabled,'environment',k.environment) FROM api_keys k WHERE k.prefix='rts_' AND k.id>$1 ORDER BY k.id LIMIT $2`)
}
func (a Management) templateIdentity(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return "", false
	}
	if r.URL.RawQuery != "" {
		respond(w, 400, "invalid_query")
		return "", false
	}
	var exists bool
	err := a.DB.QueryRowContext(r.Context(), "SELECT enabled FROM api_keys WHERE id=$1 AND prefix='rts_'", id).Scan(&exists)
	if err != nil {
		failure(w, err)
		return "", false
	}
	return id, true
}
func templateFailure(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrTemplateDraftConflict):
		respond(w, 409, "draft_revision_conflict")
	case errors.Is(err, ErrTemplateCatalog):
		respond(w, 400, "invalid_template_or_limits")
	default:
		failure(w, err)
	}
}
func (a Management) templateDraftList(w http.ResponseWriter, r *http.Request) {
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	items, err := ListTemplateDrafts(r.Context(), a.DB, id)
	if err != nil {
		templateFailure(w, err)
		return
	}
	output(w, 200, map[string]any{"items": items})
}
func (a Management) templateVersionList(w http.ResponseWriter, r *http.Request) {
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	items, err := ListPublishedTemplates(r.Context(), a.DB, id)
	if err != nil {
		templateFailure(w, err)
		return
	}
	output(w, 200, map[string]any{"items": items, "activation": "explicit_identity_opt_in_and_restart"})
}
func (a Management) templateDraftSource(w http.ResponseWriter, r *http.Request) {
	if r.Context().Value(principalKey{}).(credential).role == "viewer" {
		respond(w, 403, "forbidden")
		return
	}
	name := r.PathValue("name")
	revision, err := strconv.ParseInt(r.PathValue("revision"), 10, 64)
	if err != nil || revision < 1 || revision > 64 || strconv.FormatInt(revision, 10) != r.PathValue("revision") || !templateName(name) {
		respond(w, 400, "invalid_revision")
		return
	}
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	source, err := GetTemplateDraft(r.Context(), a.DB, id, name, revision)
	if err != nil {
		templateFailure(w, err)
		return
	}
	output(w, 200, source)
}
func (a Management) templateDraftSave(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Expected *int64          `json:"expected_revision"`
		Template json.RawMessage `json:"template"`
		Reason   string          `json:"reason"`
	}
	if !limitedBody(w, r, &v, 34<<10) {
		return
	}
	if v.Expected == nil || len(v.Template) == 0 {
		respond(w, 400, "invalid_request")
		return
	}
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	revision, replayed, err := SaveTemplateDraft(r.Context(), a.DB, id, *v.Expected, v.Template, actor(r), v.Reason)
	if err != nil {
		templateFailure(w, err)
		return
	}
	output(w, 200, map[string]any{"saved": true, "revision": revision, "replayed": replayed, "activation": "draft_only"})
}
func (a Management) templateDraftPublish(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Revision int64  `json:"revision"`
		Version  string `json:"version"`
		Reason   string `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	replayed, err := PublishTemplateDraft(r.Context(), a.DB, id, r.PathValue("name"), v.Revision, v.Version, actor(r), v.Reason)
	if err != nil {
		templateFailure(w, err)
		return
	}
	output(w, 200, map[string]any{"published": true, "replayed": replayed, "activation": "explicit_identity_opt_in_and_restart"})
}
func (a Management) templateVersionRetire(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Reason      string `json:"reason"`
		Acknowledge bool   `json:"acknowledge_retry_window"`
	}
	if !body(w, r, &v) {
		return
	}
	if !v.Acknowledge {
		respond(w, 400, "retry_window_acknowledgement_required")
		return
	}
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	replayed, err := RetireTemplate(r.Context(), a.DB, id, r.PathValue("name"), r.PathValue("version"), actor(r), v.Reason)
	if err != nil {
		templateFailure(w, err)
		return
	}
	output(w, 200, map[string]any{"retired": true, "replayed": replayed, "activation": "restart_required"})
}

// Preview is read-only: sample values are never stored, audited or enqueued.
func (a Management) templateDraftPreview(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Revision  int64           `json:"revision"`
		Version   string          `json:"version"`
		Language  string          `json:"language"`
		Variables json.RawMessage `json:"variables"`
	}
	if !body(w, r, &v) {
		return
	}
	id, ok := a.templateIdentity(w, r)
	if !ok {
		return
	}
	source, err := GetTemplateDraft(r.Context(), a.DB, id, r.PathValue("name"), v.Revision)
	if err != nil {
		templateFailure(w, err)
		return
	}
	if source.Version != v.Version || source.Language != v.Language {
		respond(w, 400, "invalid_template_preview")
		return
	}
	heads, err := ListTemplateDrafts(r.Context(), a.DB, id)
	if err != nil {
		templateFailure(w, err)
		return
	}
	current := false
	for _, head := range heads {
		if head.ID == source.ID && head.Revision == v.Revision {
			current = true
		}
	}
	if !current {
		respond(w, 409, "draft_revision_conflict")
		return
	}
	raw, err := json.Marshal(source)
	if err != nil {
		failure(w, err)
		return
	}
	preview, err := PreviewTemplateJSON(raw, v.Variables)
	if err != nil {
		respond(w, 400, "invalid_template_preview")
		return
	}
	preview["revision"] = v.Revision
	preview["identity"] = id
	preview["rendered_bytes"] = len(preview["subject"].(string)) + len(preview["text"].(string)) + len(preview["html"].(string))
	output(w, 200, preview)
}
