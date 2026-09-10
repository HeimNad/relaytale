package queue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mailgateway/internal/encryption"
	"mailgateway/internal/provider"
	"mailgateway/internal/smtpclient"
)

type Sender interface {
	Send(context.Context, provider.Provider, string, string, []smtpclient.Recipient, []byte, func(context.Context) error) smtpclient.Result
}
type Worker struct {
	Repo        Repository
	Box         *encryption.Box
	Sender      Sender
	StorageRoot string
	MaxBytes    int64
	Log         *slog.Logger
}

func (w Worker) RunOne(ctx context.Context) (bool, error) {
	return w.runOne(ctx, ctx)
}
func (w Worker) runOne(claims, ctx context.Context) (bool, error) {
	claimCtx, cancel := context.WithTimeout(claims, 10*time.Second)
	j, err := w.Repo.Claim(claimCtx)
	cancel()
	if errors.Is(err, ErrNoJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, w.deliver(ctx, j)
}
func (w Worker) deliver(ctx context.Context, j Job) (err error) {
	// A panic leaves the lease for conservative recovery; never blindly retry DATA.
	defer func() {
		if recover() != nil {
			err = errors.New("worker panic; lease retained for recovery")
		}
	}()
	operation, cancel := context.WithTimeout(ctx, j.Provider.Timeout)
	defer cancel()
	failure := func(class string) smtpclient.Result {
		now := time.Now().UTC()
		r := smtpclient.Result{Status: smtpclient.Temporary, ErrorClass: class, ErrorMessage: "delivery preparation failed", StartedAt: now, FinishedAt: now}
		for _, rc := range j.Recipients {
			r.Recipients = append(r.Recipients, smtpclient.RecipientResult{ID: rc.ID, Status: smtpclient.Temporary})
		}
		return r
	}
	password, secretErr := w.Box.Open(j.Provider.ID, j.Provider.Ciphertext, j.Provider.Nonce)
	var out smtpclient.Result
	if secretErr != nil {
		out = failure("CREDENTIAL_DECRYPTION_ERROR")
	} else {
		raw, readErr := w.readVerified(operation, j)
		if readErr != nil {
			out = failure("LOCAL_STORAGE_ERROR")
		} else {
			out = w.Sender.Send(operation, j.Provider, password, j.From, j.Recipients, raw, func(c context.Context) error { return w.Repo.ArmData(c, j) })
		}
	}
	// Persist an outcome even after network cancellation. If this fails, leave the
	// durable lease/armed marker to recovery, never attempt another provider.
	finish, cancelFinish := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFinish()
	if err = w.Repo.Finish(finish, j, out); err != nil {
		return err
	}
	w.Log.Info("delivery attempt recorded", "message_id", j.ID, "attempt_id", j.AttemptID, "provider_id", j.Provider.ID, "status", out.Status)
	return nil
}
func (w Worker) readVerified(ctx context.Context, j Job) ([]byte, error) {
	if j.Size < 0 || j.Size > w.MaxBytes {
		return nil, errors.New("invalid archive size")
	}
	rootAbs, err := filepath.Abs(w.StorageRoot)
	if err != nil {
		return nil, err
	}
	pathAbs, err := filepath.Abs(j.Path)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootAbs)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("archive must be a regular file")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, j.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != j.Size || fmt.Sprintf("%x", sha256.Sum256(raw)) != j.SHA256 {
		return nil, errors.New("archive integrity mismatch")
	}
	return raw, ctx.Err()
}

// Run stops claiming on stopClaims, lets active operations drain, and uses
// operations cancellation to interrupt them when the process grace period ends.
func (w Worker) Run(stopClaims, operations context.Context, count int) {
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if stopClaims.Err() != nil {
					return
				}
				recovery, cancel := context.WithTimeout(stopClaims, 5*time.Second)
				err := w.Repo.Recover(recovery)
				cancel()
				if err != nil && stopClaims.Err() == nil {
					w.Log.Error("lease recovery failed", "error_class", ErrorClass(err))
				}
				if stopClaims.Err() != nil {
					return
				}
				worked, err := w.runOne(stopClaims, operations)
				if err != nil {
					w.Log.Error("delivery worker failed", "error_class", ErrorClass(err))
				}
				if !worked || err != nil {
					timer := time.NewTimer(3 * time.Second)
					select {
					case <-stopClaims.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
			}
		}()
	}
	wg.Wait()
}
