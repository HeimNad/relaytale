package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"relaytale/internal/feedback"
)

// Feedback reads share management authentication, role checks and concurrency limits.
func feedbackQuery(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 8192 {
		respond(w, 400, "invalid_query")
		return nil, false
	}
	for k, v := range q {
		found := false
		for _, key := range allowed {
			if k == key {
				found = true
			}
		}
		if !found || len(v) != 1 {
			respond(w, 400, "invalid_query")
			return nil, false
		}
	}
	return q, true
}
func feedbackDigest(w http.ResponseWriter, value string) bool {
	b, e := hex.DecodeString(value)
	if e != nil || len(b) != 32 || hex.EncodeToString(b) != value {
		respond(w, 400, "invalid_digest")
		return false
	}
	return true
}
func (a Management) feedbackList(w http.ResponseWriter, r *http.Request) {
	if _, ok := feedbackQuery(w, r, "after", "limit"); !ok {
		return
	}
	after, n, ok := page(w, r)
	if !ok {
		return
	}
	items, err := (feedback.Service{DB: a.DB}).List(r.Context(), after, n+1)
	if err != nil {
		failure(w, err)
		return
	}
	more := len(items) > n
	if more {
		items = items[:n]
	}
	if items == nil {
		items = []feedback.Entry{}
	}
	last := ""
	if len(items) > 0 {
		last = items[len(items)-1].ID
	}
	output(w, 200, map[string]any{"items": items, "next_after": last, "has_more": more})
}
func (a Management) feedbackDetail(w http.ResponseWriter, r *http.Request) {
	if _, ok := feedbackQuery(w, r); !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	entry, err := (feedback.Service{DB: a.DB}).Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, entry)
}
func (a Management) feedbackExplain(w http.ResponseWriter, r *http.Request) {
	q, ok := feedbackQuery(w, r, "expected_sha256")
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok || !feedbackDigest(w, q.Get("expected_sha256")) {
		return
	}
	service := feedback.Service{DB: a.DB}
	entry, err := service.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	if entry.SHA256 != q.Get("expected_sha256") {
		respond(w, 409, "evidence_changed")
		return
	}
	result, err := service.ExplainStored(r.Context(), id, entry.SHA256)
	if err != nil {
		if errors.Is(err, feedback.ErrFormat) || errors.Is(err, feedback.ErrSize) {
			respond(w, 422, "explanation_unavailable")
		} else {
			failure(w, err)
		}
		return
	}
	output(w, 200, result)
}
func (a Management) feedbackCandidates(w http.ResponseWriter, r *http.Request) {
	q, ok := feedbackQuery(w, r, "expected_sha256", "rfc_message_id", "report_index", "limit")
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok || !feedbackDigest(w, q.Get("expected_sha256")) {
		return
	}
	index, e := strconv.Atoi(q.Get("report_index"))
	limit := 25
	if q.Has("limit") {
		var err error
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil {
			limit = 0
		}
	}
	mid := q.Get("rfc_message_id")
	if e != nil || index < 0 || index >= feedback.MaxRecipients || limit < 1 || limit > 50 || mid == "" || len(mid) > 998 || strings.TrimSpace(mid) != mid || strings.ContainsAny(mid, "\x00\r\n") {
		respond(w, 400, "invalid_query")
		return
	}
	service := feedback.Service{DB: a.DB}
	entry, err := service.Get(r.Context(), id)
	if err != nil {
		failure(w, err)
		return
	}
	if entry.SHA256 != q.Get("expected_sha256") {
		respond(w, 409, "evidence_changed")
		return
	}
	if entry.ParseState == "PARSED" {
		var report feedback.Report
		if err := json.Unmarshal(entry.Report, &report); err != nil {
			failure(w, err)
			return
		}
		if index >= len(report.Recipients) {
			respond(w, 422, "candidate_search_unavailable")
			return
		}
	}
	result, err := service.Candidates(r.Context(), feedback.CandidateRequest{FeedbackID: id, ExpectedSHA256: entry.SHA256, RFCMessageID: mid, ReportIndex: index, Limit: limit})
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, result)
}

func (a Management) feedbackCapacity(w http.ResponseWriter, r *http.Request) {
	if _, ok := feedbackQuery(w, r); !ok {
		return
	}
	value, err := (feedback.Service{DB: a.DB}).Capacity(r.Context())
	if err != nil {
		failure(w, err)
		return
	}
	output(w, 200, value)
}
