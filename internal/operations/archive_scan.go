package operations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type archiveScanCursor struct {
	Scope string `json:"scope"`
	Day   string `json:"day"`
	After string `json:"after"`
}
type ArchiveScanReport struct {
	Complete     bool                   `json:"complete"`
	NextCursor   string                 `json:"next_cursor"`
	Scanned      int                    `json:"scanned"`
	Days         int                    `json:"days"`
	Items        []ArchiveInventoryItem `json:"items"`
	Observations []ArchiveObservation   `json:"observations"`
}

// ScanArchives spans an explicit UTC date range. It records candidates only
// when requested. Each call bounds results and visits at most 31 date slots.
func ScanArchives(ctx context.Context, db *sql.DB, root, from, to, cursor string, limit int, record bool) (report ArchiveScanReport, err error) {
	report.Items = []ArchiveInventoryItem{}
	report.Observations = []ArchiveObservation{}
	start, e := time.Parse("2006-01-02", from)
	if e != nil {
		return report, e
	}
	end, e := time.Parse("2006-01-02", to)
	if e != nil || end.Before(start) || end.Sub(start) > 36600*24*time.Hour || root == "" || limit < 1 || limit > 10000 || len(cursor) > 2048 {
		return report, errors.New("invalid scan bounds")
	}
	absolute, e := filepath.Abs(root)
	if e != nil {
		return report, e
	}
	digest := sha256.Sum256([]byte(absolute + "\x00" + from + "\x00" + to + "\x00" + strconv.FormatBool(record)))
	state := archiveScanCursor{Scope: hex.EncodeToString(digest[:]), Day: from}
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil {
			return report, e
		}
		var saved archiveScanCursor
		if e = json.Unmarshal(raw, &saved); e != nil || saved.Scope != state.Scope {
			return report, errors.New("cursor scope mismatch")
		}
		date, e := time.Parse("2006-01-02", saved.Day)
		if e != nil || date.Before(start) || date.After(end) {
			return report, errors.New("invalid cursor date")
		}
		state = saved
	}
	date, _ := time.Parse("2006-01-02", state.Day)
	dir, e := os.OpenRoot(absolute)
	if e != nil {
		return report, e
	}
	defer dir.Close()
	for !date.After(end) && report.Scanned < limit && report.Days < 31 {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		report.Days++
		// Missing dates are normal in sparse archives. Other filesystem errors fail.
		_, e = dir.Lstat(date.Format("2006/01/02"))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return report, e
		}
		if e == nil {
			page, e := InventoryArchivesPage(ctx, db, absolute, date.Format("2006-01-02"), state.After, limit-report.Scanned)
			if e != nil {
				return report, e
			}
			report.Scanned += page.Scanned
			report.Items = append(report.Items, page.Items...)
			if record {
				for _, item := range page.Items {
					if item.Reason != "NO_VISIBLE_REFERENCE_REVIEW" {
						continue
					}
					observation, e := ObserveArchive(ctx, db, absolute, item.Path)
					if e != nil {
						return report, e
					}
					report.Observations = append(report.Observations, observation)
				}
			}
			if page.Truncated {
				state.After = page.NextAfter
				break
			}
		}
		date = date.AddDate(0, 0, 1)
		state.Day = date.Format("2006-01-02")
		state.After = ""
	}
	report.Complete = date.After(end)
	if !report.Complete {
		raw, _ := json.Marshal(state)
		report.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return report, nil
}

type ArchiveObservationStatus struct {
	ArchiveObservation
	ObservedAt  time.Time `json:"observed_at"`
	EligibleAge bool      `json:"eligible_age"`
	Retired     bool      `json:"retired"`
	Removed     bool      `json:"removed"`
}

func ListArchiveObservations(ctx context.Context, db *sql.DB, root, after string, limit int) ([]ArchiveObservationStatus, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("invalid list limit")
	}
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT o.id,o.archive_id,o.path,o.size,o.sha256,o.observed_at,o.observed_at<=clock_timestamp()-interval '24 hours',r.archive_id IS NOT NULL,c.archive_id IS NOT NULL FROM archive_observations o LEFT JOIN archive_retirements r ON r.observation_id=o.id LEFT JOIN archive_reclaims c ON c.archive_id=r.archive_id WHERE o.root=$1 AND o.id>$2::uuid ORDER BY o.id LIMIT $3`, absolute, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArchiveObservationStatus{}
	for rows.Next() {
		var v ArchiveObservationStatus
		if err = rows.Scan(&v.ID, &v.ArchiveID, &v.Path, &v.Size, &v.SHA256, &v.ObservedAt, &v.EligibleAge, &v.Retired, &v.Removed); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
