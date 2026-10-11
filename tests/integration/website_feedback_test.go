package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"relaytale/examples/website/site"
	"relaytale/internal/feedback"
	"relaytale/internal/testsmtp"
	"relaytale/internal/webhook"
)

// Only the test sender redirects the destination to loopback. Production's
// HTTPSender retains its public-address and TLS requirements.
type websiteAcceptanceSender struct{ URL string }

func (s websiteAcceptanceSender) Send(ctx context.Context, endpoint webhook.Endpoint, raw []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", s.URL+"/relaytale/hooks", bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	stamp, signature := webhook.Sign(endpoint.Keys, time.Now(), raw)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-RelayTale-Timestamp", stamp)
	req.Header.Set("X-RelayTale-Signature", signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

func TestWebsiteProductionManualFeedbackSignedReconciliation(t *testing.T) {
	ctx := context.Background()
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	gateway := httptest.NewServer(h)
	defer gateway.Close()
	keys := []webhook.Key{{ID: "current", Secret: strings.Repeat("ab", 32)}}
	config := site.Config{DB: f.db, GatewayURL: gateway.URL, GatewayToken: key.Token, DemoToken: strings.Repeat("d", 32), Keys: keys, Environment: "production", AcknowledgeProduction: true}
	unacknowledged := config
	unacknowledged.AcknowledgeProduction = false
	if _, err := site.New(ctx, unacknowledged); err == nil {
		t.Fatal("production enabled without acknowledgement")
	}
	app, err := site.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(app)
	defer server.Close()
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/relaytale/hooks", Keys: keys, SchemaVersion: 2}
	if err = webhook.Sync(ctx, f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	request := site.Submission{From: key.AllowedFrom[0], To: []string{"one@example.test"}, MIME: f.raw()}
	// Defaults must still refuse a production identity before sending any mail.
	defaults := config
	defaults.Environment = ""
	defaults.AcknowledgeProduction = false
	sandboxApp, err := site.New(ctx, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sandboxApp.Submit(ctx, uuid.NewString(), request); !errors.Is(err, site.ErrUnavailable) {
		t.Fatal("default sandbox accepted production identity", err)
	}
	assertCount(t, f, 0, `SELECT count(*) FROM messages WHERE api_key_id=$1`, key.ID)
	record, err := app.Submit(ctx, uuid.NewString(), request)
	if err != nil || record.MessageID == nil {
		t.Fatal(record, err)
	}
	mid := *record.MessageID
	runOne(t, f.worker)
	s := feedback.Service{DB: f.db}
	source := uuid.NewString()
	imported, err := s.Import(ctx, feedback.ImportRequest{Source: source, ExternalID: "website-bounce", Actor: "test", Reason: "synthetic acceptance evidence"}, strings.NewReader(feedbackRaw("one@example.test")))
	if err != nil {
		t.Fatal(err)
	}
	linkRequest := feedback.LinkRequest{InspectRequest: feedback.InspectRequest{FeedbackID: imported.Entry.ID, ExpectedSHA256: imported.Entry.SHA256, MessageID: mid, ProviderID: providerID(t, f)}, Reason: "operator compared synthetic evidence", AcknowledgeUnverified: true}
	if err = f.db.QueryRow(`SELECT r.id,a.id FROM recipients r JOIN delivery_attempts a ON a.message_id=r.message_id WHERE r.message_id=$1`, mid).Scan(&linkRequest.RecipientID, &linkRequest.AttemptID); err != nil {
		t.Fatal(err)
	}
	admin := management(t, f)
	linked := adminRequest(admin, "POST", "/feedback/"+imported.Entry.ID+"/links", "a", linkRequest)
	requireStatus(t, linked, 201)
	var publication feedback.Publication
	if err = json.Unmarshal(linked.Body.Bytes(), &publication); err != nil {
		t.Fatal(err)
	}
	sender := websiteAcceptanceSender{URL: server.URL}
	worker := webhook.Worker{DB: f.db, Sender: sender}
	check := func(state string) {
		t.Helper()
		for i := 0; ; i++ {
			if i >= 100 {
				t.Fatal("outbox did not drain")
			}
			did, e := worker.RunOne(ctx, endpoint)
			if e != nil {
				t.Fatal(e)
			}
			if !did {
				break
			}
		}
		assertCount(t, f, 0, `SELECT count(*) FROM webhook_outbox WHERE endpoint_id=$1 AND state<>'DELIVERED'`, endpoint.ID)
		if e := app.ReconcilePending(ctx); e != nil {
			t.Fatal(e)
		}
		current, e := app.Get(ctx, record.ID)
		if e != nil || current.NeedsRefresh || !strings.Contains(string(current.Feedback), state) || !strings.Contains(string(current.Snapshot), "SMTP_ACCEPTED") || !strings.Contains(string(current.Feedback), "UNVERIFIED") {
			t.Fatal(current, e)
		}
		if f.status(t, mid) != "SMTP_ACCEPTED" {
			t.Fatal("feedback changed SMTP acceptance fact")
		}
	}
	check("PENDING_REVIEW")
	review := feedback.ReviewRequest{Action: "CONFIRM_FAILURE", Reason: "operator inspected synthetic failure", AcknowledgeUnverified: true}
	requireStatus(t, adminRequest(admin, "POST", "/feedback-links/"+publication.ID+"/reviews", "a", review), 200)
	check("FAILURE_CONFIRMED_BY_OPERATOR")
	var oldPayload string
	if err = f.db.QueryRow(`SELECT payload FROM webhook_outbox WHERE endpoint_id=$1 AND payload::jsonb->>'type'='FEEDBACK_LINKED'`, endpoint.ID).Scan(&oldPayload); err != nil {
		t.Fatal(err)
	}
	// Replay an older signed notification after confirmation. Deduplication and
	// fresh-query reconciliation must preserve the newer effective conclusion.
	if code, e := sender.Send(ctx, endpoint, []byte(oldPayload)); e != nil || code != 204 {
		t.Fatal(code, e)
	}
	check("FAILURE_CONFIRMED_BY_OPERATOR")
	review.ExpectedRevision = 1
	review.Action = "WITHDRAW"
	requireStatus(t, adminRequest(admin, "POST", "/feedback-links/"+publication.ID+"/reviews", "a", review), 200)
	check("WITHDRAWN")
	// Restarting the website must preserve both snapshots and the durable inbox.
	app, err = site.New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	check("WITHDRAWN")
	_, err = s.Import(ctx, feedback.ImportRequest{Source: source, ExternalID: "website-bounce", Actor: "test", Reason: "late conflicting synthetic evidence"}, strings.NewReader(strings.Replace(feedbackRaw("one@example.test"), "5.1.1", "5.1.2", 1)))
	if err != nil {
		t.Fatal(err)
	}
	check("EVIDENCE_CONFLICT")
}
