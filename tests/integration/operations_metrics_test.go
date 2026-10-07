package integration

import (
	"net/http/httptest"
	"strings"
	"testing"

	"relaytale/internal/metrics"
	"relaytale/internal/resource"
	"relaytale/internal/testsmtp"
)

func TestMetricsSharedQuotaCountsDisabledHistory(t *testing.T) {
	_, url := recoveryDB(t)
	t.Setenv("TEST_DATABASE_URL", url)
	f := delivery(t, testsmtp.Options{DropFinal: true}, 1)
	first := providerID(t, f)
	group := quotaGroup(t, f, 2, 10)
	attachGroup(t, f, first, group)
	f.seed(t)
	runOne(t, f.worker)
	second := f.addProvider(t, f.fake, 20, 1)
	attachGroup(t, f, second, group)
	mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, first)
	scrape := func(want string) {
		t.Helper()
		h := &metrics.Handler{DB: f.db, Resources: resource.New(64<<20, 256<<20)}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatal("metric mismatch", w.Code, w.Body.String())
		}
		for _, private := range []string{first, second, group, f.domain, "@"} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatal("private metric label")
			}
		}
	}
	scrape("relaytale_recipients_delivery_unknown 2\n")
	scrape("relaytale_providers_quota_exhausted 1\n")
	scrape("relaytale_quota_groups_exhausted 1\n")
	mustExec(t, f, `UPDATE provider_quota SET reserved_at=clock_timestamp()-interval '25 hours' WHERE provider_id=$1`, first)
	scrape("relaytale_providers_quota_exhausted 0\n")
	scrape("relaytale_quota_groups_exhausted 0\n")
	mustExec(t, f, `UPDATE providers SET enabled=false WHERE id=$1`, second)
	scrape("relaytale_providers_enabled 0\n")
	scrape("relaytale_quota_groups_active 0\n")
}
