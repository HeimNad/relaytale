package integration

import (
	"context"
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

// This helper is only installed by the explicitly enabled browser fixture.
func browserWebsiteFeedback(t *testing.T, fallback http.Handler) http.Handler {
	t.Helper()
	f := delivery(t, testsmtp.Options{}, 1)
	h, key, _ := sendFixture(t, f)
	gateway := httptest.NewServer(h)
	t.Cleanup(gateway.Close)
	config := site.Config{DB: f.db, GatewayURL: gateway.URL, GatewayToken: key.Token, DemoToken: strings.Repeat("d", 32), Keys: []webhook.Key{{ID: "browser", Secret: strings.Repeat("ab", 32)}}, Environment: "production", AcknowledgeProduction: true}
	app, err := site.New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	website := httptest.NewServer(app)
	t.Cleanup(website.Close)
	endpoint := webhook.Endpoint{ID: uuid.NewString(), APIKeyID: key.ID, URL: "https://example.com/hooks", Keys: config.Keys, SchemaVersion: 2}
	if err = webhook.Sync(context.Background(), f.db, []webhook.Endpoint{endpoint}); err != nil {
		t.Fatal(err)
	}
	record, err := app.Submit(context.Background(), uuid.NewString(), site.Submission{From: key.AllowedFrom[0], To: []string{"one@example.test"}, MIME: f.raw()})
	if err != nil || record.MessageID == nil {
		t.Fatal(record, err)
	}
	// Other fixture messages stay queued; only this website message is sent.
	mustExec(t, f, `UPDATE messages SET priority=32767,message_id='<website-browser@example.test>' WHERE id=$1`, *record.MessageID)
	runOne(t, f.worker)
	if f.status(t, *record.MessageID) != "SMTP_ACCEPTED" {
		t.Fatal("website fixture was not accepted")
	}
	_, err = (feedback.Service{DB: f.db}).Import(context.Background(), feedback.ImportRequest{Source: "网站全链路退信样例", Actor: "browser-test", Reason: "synthetic website acceptance evidence"}, strings.NewReader(feedbackRaw("one@example.test")))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	errors := make(chan error, 1)
	t.Cleanup(func() {
		cancel()
		<-done
		select {
		case err := <-errors:
			t.Error(err)
		default:
		}
	})
	go func() {
		defer close(done)
		worker := webhook.Worker{DB: f.db, Sender: websiteAcceptanceSender{URL: website.URL}}
		// Batch notifications before the two-read reconciliation, respecting the
		// real per-identity one-request/second budget rather than weakening it.
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var err error
				for i := 0; i < 100; i++ {
					var did bool
					did, err = worker.RunOne(ctx, endpoint)
					if err != nil || !did {
						break
					}
				}
				if err == nil {
					err = app.ReconcilePending(ctx)
				}
				if err != nil && ctx.Err() == nil {
					errors <- err
					return
				}
			}
		}
	}()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/e2e/website" {
			fallback.ServeHTTP(w, r)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), "GET", website.URL+"/demo/submissions/"+record.ID, nil)
		if err != nil {
			http.Error(w, "fixture request failed", 500)
			return
		}
		request.Header.Set("Authorization", "Bearer "+config.DemoToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, "fixture unavailable", 503)
			return
		}
		defer response.Body.Close()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
	})
}
