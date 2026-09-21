package feedback

import "context"

const MaxEvidenceRecords int64 = 100000
const MaxEvidenceBytes int64 = 1 << 30

// EvidenceCapacity accounts logical raw and parsed bytes, not PostgreSQL disk use.
// These limits are enforced in migration 00011 for every database writer.
type EvidenceCapacity struct {
	Records     int64 `json:"records"`
	StoredBytes int64 `json:"stored_bytes"`
	MaxRecords  int64 `json:"max_records"`
	MaxBytes    int64 `json:"max_bytes"`
	AtLimit     bool  `json:"at_limit"`
}

func (s Service) Capacity(ctx context.Context) (EvidenceCapacity, error) {
	out := EvidenceCapacity{MaxRecords: MaxEvidenceRecords, MaxBytes: MaxEvidenceBytes}
	err := s.DB.QueryRowContext(ctx, `SELECT records,stored_bytes FROM feedback_capacity WHERE singleton`).Scan(&out.Records, &out.StoredBytes)
	out.AtLimit = out.Records >= out.MaxRecords || out.StoredBytes >= out.MaxBytes
	return out, err
}
