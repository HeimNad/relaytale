package feedback

import (
	"context"
	"errors"
	"time"
)

type CollectionReceipt struct {
	UID          uint32 `json:"uid"`
	Outcome      string `json:"outcome"`
	ReportedSize int64  `json:"reported_size"`
	FeedbackID   string `json:"feedback_id,omitempty"`
}
type CollectionAlert struct {
	UIDValidity uint32    `json:"uid_validity"`
	Kind        string    `json:"kind"`
	CreatedAt   time.Time `json:"created_at"`
}
type CollectionStatus struct {
	Source        string              `json:"source"`
	UIDValidity   uint32              `json:"uid_validity"`
	LastUID       uint32              `json:"last_uid"`
	StoredBytes   int64               `json:"stored_bytes"`
	Records       int                 `json:"records"`
	ReceiptsEpoch uint32              `json:"receipts_epoch"`
	Receipts      []CollectionReceipt `json:"receipts"`
	LatestAlerts  []CollectionAlert   `json:"latest_alerts"`
}

// CollectionStatus provides bounded receipt pagination; alerts are the latest 20.
func (s Service) CollectionStatus(ctx context.Context, source string, epoch, after uint32, limit int) (CollectionStatus, error) {
	out := CollectionStatus{Source: source, Receipts: []CollectionReceipt{}, LatestAlerts: []CollectionAlert{}}
	if !sourceName.MatchString(source) || limit < 1 || limit > 100 {
		return out, errors.New("invalid source or limit")
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT uid_validity,last_uid,stored_bytes,records FROM feedback_imap_sources WHERE name=$1`, source).Scan(&out.UIDValidity, &out.LastUID, &out.StoredBytes, &out.Records); err != nil {
		return out, err
	}
	if epoch == 0 {
		epoch = out.UIDValidity
	}
	out.ReceiptsEpoch = epoch
	rows, err := s.DB.QueryContext(ctx, `SELECT uid,outcome,reported_size,COALESCE(feedback_id::text,'') FROM feedback_imap_receipts WHERE source=$1 AND uid_validity=$2 AND uid>$3 ORDER BY uid LIMIT $4`, source, epoch, after, limit)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r CollectionReceipt
		if err = rows.Scan(&r.UID, &r.Outcome, &r.ReportedSize, &r.FeedbackID); err != nil {
			rows.Close()
			return out, err
		}
		out.Receipts = append(out.Receipts, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = s.DB.QueryContext(ctx, `SELECT uid_validity,kind,created_at FROM feedback_imap_alerts WHERE source=$1 ORDER BY created_at DESC LIMIT 20`, source)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var a CollectionAlert
		if err = rows.Scan(&a.UIDValidity, &a.Kind, &a.CreatedAt); err != nil {
			return out, err
		}
		out.LatestAlerts = append(out.LatestAlerts, a)
	}
	return out, rows.Err()
}
