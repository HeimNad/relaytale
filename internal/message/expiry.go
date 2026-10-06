package message

import (
	"errors"
	"time"
)

var ErrExpiredSubmission = errors.New("submission deadline elapsed")
var ErrInvalidExpiry = errors.New("expiry must be within 30 days")

// CanonicalExpiry uses whole UTC seconds so PostgreSQL and request fingerprints
// represent exactly the same instant. Zero preserves legacy requests.
func CanonicalExpiry(at time.Time) (string, error) {
	if at.IsZero() {
		return "", nil
	}
	_, offset := at.Zone()
	if at.UTC().Year() < 1 || at.UTC().Year() > 9999 || at.Nanosecond() != 0 || offset <= -24*3600 || offset >= 24*3600 {
		return "", ErrInvalidSubmission
	}
	return at.UTC().Format(time.RFC3339), nil
}
