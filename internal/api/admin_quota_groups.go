package api

import (
	"net/http"
	"relaytale/internal/provider"
)

func (a Management) quotaGroups(w http.ResponseWriter, r *http.Request) {
	a.list(w, r, `SELECT g.id,to_jsonb(g) FROM provider_quota_group_usage g WHERE g.id>$1 ORDER BY g.id LIMIT $2`)
}
func (a Management) quotaGroup(w http.ResponseWriter, r *http.Request) {
	a.one(w, r, `SELECT to_jsonb(g) FROM provider_quota_group_usage g WHERE g.id=$1`)
}
func (a Management) writeQuotaGroup(w http.ResponseWriter, r *http.Request) {
	var v struct {
		ID       string                      `json:"id"`
		Expected int64                       `json:"expected_revision"`
		Settings provider.QuotaGroupSettings `json:"settings"`
		Reason   string                      `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	create := r.Method == "POST"
	if !create {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if v.ID != "" && v.ID != id {
			respond(w, 400, "invalid_request")
			return
		}
		v.ID = id
	}
	rev, err := provider.ManageQuotaGroup(r.Context(), a.DB, create, v.ID, v.Expected, v.Settings, actor(r), v.Reason)
	if err != nil {
		failure(w, err)
		return
	}
	code := 200
	if create {
		code = 201
	}
	output(w, code, map[string]any{"id": v.ID, "revision": rev})
}
func (a Management) attachQuotaGroup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var v struct {
		GroupID  string `json:"group_id"`
		Expected int64  `json:"expected_revision"`
		Reason   string `json:"reason"`
	}
	if !body(w, r, &v) {
		return
	}
	rev, err := provider.AttachQuotaGroup(r.Context(), a.DB, id, v.GroupID, v.Expected, actor(r), v.Reason)
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, map[string]any{"id": id, "quota_group_id": v.GroupID, "revision": rev})
}
