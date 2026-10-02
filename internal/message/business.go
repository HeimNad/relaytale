package message

import (
	"regexp"
	"slices"
)

// Business contains only opaque correlation identifiers, never general-purpose
// customer metadata. It has no routing, authorization or priority semantics.
type Business struct {
	ClientReference string   `json:"client_reference"`
	Tags            []string `json:"tags"`
}

var referencePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

func ValidReference(s string) bool { return referencePattern.MatchString(s) }
func ValidTag(s string) bool       { return tagPattern.MatchString(s) }
func (b Business) Present() bool   { return b.ClientReference != "" || len(b.Tags) > 0 }
func (b Business) Canonical() (Business, error) {
	if b.ClientReference != "" && !ValidReference(b.ClientReference) || len(b.Tags) > 8 {
		return Business{}, ErrInvalidSubmission
	}
	out := Business{ClientReference: b.ClientReference, Tags: append([]string{}, b.Tags...)}
	slices.Sort(out.Tags)
	for i, tag := range out.Tags {
		if !ValidTag(tag) || i > 0 && tag == out.Tags[i-1] {
			return Business{}, ErrInvalidSubmission
		}
	}
	return out, nil
}
