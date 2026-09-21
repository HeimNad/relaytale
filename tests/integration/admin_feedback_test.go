package integration

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"relaytale/internal/feedback"
	"relaytale/internal/testsmtp"
)

func TestManagementFeedbackReadOnly(t *testing.T) {
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	address := "one@" + f.domain
	mid := suppressionSeed(t, f, address)
	hint := "<admin-feedback@example.test>"
	mustExec(t, f, `UPDATE messages SET message_id=$2 WHERE id=$1`, mid, hint)
	runOne(t, f.worker)
	s := feedback.Service{DB: f.db}
	entry, err := s.Import(context.Background(), feedback.ImportRequest{Source: "admin-test", Actor: "test", Reason: "read-only verification"}, strings.NewReader(feedbackRaw(address)))
	if err != nil {
		t.Fatal(err)
	}
	h := management(t, f)
	path := "/feedback/" + entry.Entry.ID
	digest := "expected_sha256=" + entry.Entry.SHA256
	for _, p := range []string{"/feedback?limit=1", path, path + "/explanation?" + digest, path + "/candidates?" + digest + "&report_index=0&rfc_message_id=" + url.QueryEscape(hint)} {
		w := adminRequest(h, "GET", p, "v", nil)
		requireStatus(t, w, 200)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("feedback cacheable")
		}
		if strings.Contains(p, "candidates") && (!strings.Contains(w.Body.String(), "DELIVERY_UNKNOWN_PRESERVED") || !strings.Contains(w.Body.String(), `"automatic_action_allowed":false`)) {
			t.Fatal(w.Body)
		}
		requireStatus(t, adminRequest(h, "GET", p, "x", nil), 401)
	}
	for _, p := range []string{"/feedback?limit=101", "/feedback?limit=1&limit=2", "/feedback?bad=%zz", path + "?raw=true", path + "/explanation?expected_sha256=bad", path + "/candidates?" + digest + "&report_index=-1&rfc_message_id=x", path + "/candidates?" + digest + "&report_index=0&rfc_message_id=x&limit=51"} {
		requireStatus(t, adminRequest(h, "GET", p, "v", nil), 400)
	}
	requireStatus(t, adminRequest(h, "GET", path+"/explanation?expected_sha256="+strings.Repeat("0", 64), "v", nil), 409)
	requireStatus(t, adminRequest(h, "POST", path+"/candidates", "v", nil), 403)
	requireStatus(t, adminRequest(h, "GET", path+"/candidates?"+digest+"&report_index=99&rfc_message_id=x", "v", nil), 422)
	if f.status(t, mid) != "DELIVERY_UNKNOWN" {
		t.Fatal("read changed delivery")
	}
	assertCount(t, f, 0, `SELECT count(*) FROM suppression_entries WHERE email=$1`, address)
	assertCount(t, f, 1, `SELECT count(*) FROM maintenance_audit WHERE data->>'id'=$1`, entry.Entry.ID)
}
