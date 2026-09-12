package integration

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"relaytale/internal/auth"
	"relaytale/internal/message"
	"relaytale/internal/metrics"
	"relaytale/internal/queue"
	"relaytale/internal/resource"
	"relaytale/internal/smtpclient"
	"relaytale/internal/storage"
	"relaytale/internal/testsmtp"
)

// bodyReader generates canonical, dot-leading SMTP lines without a large buffer.
type bodyReader struct {
	remaining int64
	line      [1000]byte
	n, pos    int
}

func (r *bodyReader) Read(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		if r.pos == r.n {
			if r.remaining == 0 {
				if total > 0 {
					return total, nil
				}
				return 0, io.EOF
			}
			n := int64(1000)
			if r.remaining < n {
				n = r.remaining
			}
			if r.remaining-n == 1 {
				n--
			}
			if n < 2 {
				return total, fmt.Errorf("invalid fixture tail")
			}
			r.n = int(n)
			r.pos = 0
			r.remaining -= n
			for i := 0; i < r.n; i++ {
				r.line[i] = 'x'
			}
			r.line[0] = '.'
			r.line[r.n-2] = '\r'
			r.line[r.n-1] = '\n'
		}
		n := copy(p, r.line[r.pos:r.n])
		r.pos += n
		total += n
		p = p[n:]
	}
	return total, nil
}
func generatedMessage(f *deliveryFixture, size int64) io.Reader {
	header := "From: sender@" + f.domain + "\r\nTo: recipient@example.test\r\nSubject: Resource validation\r\nMessage-ID: <resource-test@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"
	return io.MultiReader(strings.NewReader(header), &bodyReader{remaining: size - int64(len(header))})
}
func receiveSized(ctx context.Context, f *deliveryFixture, limits *resource.Limiter, size, max int64) (string, error) {
	receiver := message.Receiver{Resources: limits, Store: storage.LocalStore{Root: f.root, MaxBytes: max}, Repo: message.Postgres{DB: f.db}}
	return receiver.Receive(ctx, message.Envelope{Account: auth.Account{ID: f.accountID, AllowedFrom: []string{"sender@" + f.domain}}, From: "sender@" + f.domain, Recipients: []string{"recipient@" + f.domain}}, generatedMessage(f, size))
}
func TestLargeMessagesBoundedAndByteIdentical(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int64{}
	f := delivery(t, testsmtp.Options{DiscardPayload: true, MaxBytes: 25 << 20, ReadDelay: time.Millisecond, OnMessage: func(n int64, hash string) { mu.Lock(); got[hash] = n; mu.Unlock() }}, 4)
	limits := resource.New(2*resource.SendMemory, 50<<20)
	f.worker.Resources = limits
	f.worker.MaxBytes = 25 << 20
	f.worker.SnapshotDir = t.TempDir()
	mustExec(t, f, `UPDATE providers SET timeout_seconds=30 WHERE id=$1`, providerID(t, f))
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	baselineRSS := rssBytes()
	started := time.Now()
	var peakHeap, peakRSS uint64
	stopSample, sampleDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSample:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peakHeap {
					peakHeap = m.HeapAlloc
				}
				if rss := rssBytes(); rss > peakRSS {
					peakRSS = rss
				}
			}
		}
	}()
	defer func() {
		close(stopSample)
		<-sampleDone
		if peakHeap > 128<<20 {
			t.Error("large fixture exceeded declared 128 MiB sampled Go heap bound")
		}
		t.Logf("LARGE_REPORT baseline_heap=%d peak_heap=%d baseline_rss=%d peak_rss=%d elapsed_seconds=%.3f", baseline.HeapAlloc, peakHeap, baselineRSS, peakRSS, time.Since(started).Seconds())
	}()
	ids := []string{}
	for _, size := range []int64{16 << 20, 25 << 20} {
		id, err := receiveSized(context.Background(), f, limits, size, 25<<20)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				worked, err := f.worker.RunOne(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if worked {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if f.status(t, id) != "SMTP_ACCEPTED" {
			t.Fatal("large message not accepted")
		}
		var hash string
		var size int64
		if err := f.db.QueryRow(`SELECT eml_sha256,eml_size FROM messages WHERE id=$1`, id).Scan(&hash, &size); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		received := got[hash]
		mu.Unlock()
		if received != size {
			t.Fatal("large EML bytes changed", received, size)
		}
	}
	s := limits.Stats()
	if s.Memory != 0 || s.Spool != 0 || s.PeakMemory > s.MemoryLimit || s.PeakSpool > s.SpoolLimit {
		t.Fatal("resource leak/oversubscription", s)
	}
	files, err := os.ReadDir(f.worker.SnapshotDir)
	if err != nil || len(files) != 0 {
		t.Fatal("snapshot leak", err)
	}
	if _, err = receiveSized(context.Background(), f, limits, (25<<20)+1, 25<<20); err == nil {
		t.Fatal("oversize ingress accepted")
	}
	assertCount(t, f, 2, `SELECT count(*) FROM messages WHERE smtp_account_id=$1`, f.accountID)
}
func TestLargeDisconnectAndLostFinalStayUnknown(t *testing.T) {
	for _, mode := range []string{"mid-data", "lost-final"} {
		t.Run(mode, func(t *testing.T) {
			options := testsmtp.Options{DiscardPayload: true, MaxBytes: 25 << 20}
			if mode == "mid-data" {
				options.DropAfterBytes = 1 << 20
			} else {
				options.DropFinal = true
			}
			f := delivery(t, options, 1)
			f.worker.MaxBytes = 25 << 20
			mustExec(t, f, `UPDATE providers SET timeout_seconds=30 WHERE id=$1`, providerID(t, f))
			id, err := receiveSized(context.Background(), f, nil, 16<<20, 25<<20)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			worked, err := f.worker.RunOne(ctx)
			if !worked || err != nil {
				t.Fatal(worked, err)
			}
			if f.status(t, id) != "DELIVERY_UNKNOWN" {
				t.Fatal("unsafe large message result")
			}
			noWork(t, f)
		})
	}
}
func TestSnapshotDiskFailureLeavesManualHold(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	id := f.seed(t)
	f.worker.SnapshotDir = t.TempDir() + "/absent/dir"
	runOne(t, f.worker)
	if f.status(t, id) != "TEMP_FAILED" {
		t.Fatal("local failure not held")
	}
	assertCount(t, f, 1, `SELECT count(*) FROM delivery_attempts WHERE message_id=$1 AND error_class='LOCAL_STORAGE_ERROR' AND data_armed_at IS NULL`, id)
	noWork(t, f)
}
func TestMetricsAggregateNoPrivateLabels(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 1)
	f.seed(t)
	runOne(t, f.worker)
	h := &metrics.Handler{DB: f.db, Resources: resource.New(64<<20, 256<<20)}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "relaytale_metrics_scrape_success 1") || !strings.Contains(body, "relaytale_queue_oldest_seconds") {
		t.Fatal("bad metrics scrape", w.Code, body)
	}
	for _, secret := range []string{f.domain, f.accountID, "@", "provider-password"} {
		if strings.Contains(body, secret) {
			t.Fatal("private/high-cardinality metric")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil).WithContext(ctx))
	if w.Code != 503 || w.Body.String() != "relaytale_metrics_scrape_success 0\n" {
		t.Fatal("failed scrape leaked state", w.Body.String())
	}
}

func TestLargeCancellationStaysUnknown(t *testing.T) {
	f := delivery(t, testsmtp.Options{DiscardPayload: true, MaxBytes: 25 << 20, ReadDelay: 20 * time.Millisecond}, 1)
	f.worker.MaxBytes = 25 << 20
	mustExec(t, f, `UPDATE providers SET timeout_seconds=30 WHERE id=$1`, providerID(t, f))
	id, err := receiveSized(context.Background(), f, nil, 16<<20, 25<<20)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.worker.Sender = suppressionSender{f.worker.Sender, func(c context.Context, gate func(context.Context) error) error {
		if err := gate(c); err != nil {
			return err
		}
		time.AfterFunc(100*time.Millisecond, cancel)
		return nil
	}}
	worked, err := f.worker.RunOne(ctx)
	if !worked || err != nil {
		t.Fatal(worked, err)
	}
	if f.status(t, id) != "DELIVERY_UNKNOWN" {
		t.Fatal("cancelled armed transmission must stay unknown")
	}
	noWork(t, f)
	if snapshotFDs() != 0 {
		t.Fatal("snapshot descriptor leaked")
	}
}

// Hold a compatible provider lock until both finishes reach the exclusive lock.
// Previously both first acquired FK KEY SHARE via recipients.last_provider_id,
// then deadlocked while upgrading to FOR UPDATE after this blocker released.
func TestConcurrentFinishLocksProviderBeforeRecipientForeignKeys(t *testing.T) {
	f := delivery(t, testsmtp.Options{}, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobs := make([]queue.Job, 2)
	for i := range jobs {
		f.seed(t)
		j, err := f.worker.Repo.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.worker.Repo.ArmData(ctx, j); err != nil {
			t.Fatal(err)
		}
		jobs[i] = j
	}
	gate, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var pid int
	if err = gate.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = gate.ExecContext(ctx, `SELECT id FROM providers WHERE id=$1 FOR NO KEY UPDATE`, jobs[0].Provider.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	for _, j := range jobs {
		go func(j queue.Job) {
			now := time.Now().UTC()
			out := smtpclient.Result{Status: smtpclient.Accepted, Code: 250, Stage: "FINAL_RESPONSE", StartedAt: now, FinishedAt: now, DataStartedAt: now, DataCompletedAt: now, FinalResponseAt: now}
			for _, rc := range j.Recipients {
				out.Recipients = append(out.Recipients, smtpclient.RecipientResult{ID: rc.ID, Status: smtpclient.Accepted, Code: 250})
			}
			done <- f.worker.Repo.Finish(ctx, j, out)
		}(j)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err = f.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>$1 AND wait_event_type='Lock' AND query LIKE '%FROM providers WHERE id=%FOR UPDATE%'`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finishes did not reach provider lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	for range jobs {
		if err = <-done; err != nil {
			t.Fatal("concurrent finish failed", err)
		}
	}
	for _, j := range jobs {
		if f.status(t, j.ID) != smtpclient.Accepted {
			t.Fatal("accepted result lost")
		}
	}
	assertCount(t, f, 2, `SELECT count(*) FROM delivery_attempts WHERE provider_id=$1 AND result='SMTP_ACCEPTED' AND health_outcome='SUCCESS'`, jobs[0].Provider.ID)
}
