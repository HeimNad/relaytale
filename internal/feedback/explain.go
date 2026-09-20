package feedback

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

type FormatIssue struct {
	Block int    `json:"block"`
	Code  string `json:"code"`
	Field string `json:"field,omitempty"`
}
type FieldHints struct {
	Mailbox  string `json:"mailbox,omitempty"`
	SMTPCode int    `json:"smtp_code,omitempty"`
}
type ExplainedBlock struct {
	Index  int                 `json:"index"`
	Fields map[string][]string `json:"fields"`
	Hints  FieldHints          `json:"hints"`
}
type Explanation struct {
	Version                string           `json:"version"`
	SHA256                 string           `json:"sha256"`
	Trust                  string           `json:"trust"`
	AutomaticActionAllowed bool             `json:"automatic_action_allowed"`
	StrictlyParsed         bool             `json:"strictly_parsed"`
	RawStatus              string           `json:"raw_status"`
	Blocks                 []ExplainedBlock `json:"blocks"`
	Issues                 []FormatIssue    `json:"issues"`
}

// Explain is an advisory view, deliberately not a Report or a state transition.
// RawStatus preserves original line endings/folding; Fields unfolds headers.
func Explain(raw []byte) (Explanation, error) {
	body, err := statusBody(raw)
	if err != nil {
		return Explanation{}, err
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return Explanation{}, ErrFormat
	}
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if bytes.IndexByte(normalized, '\r') >= 0 {
		return Explanation{}, ErrFormat
	}
	for _, line := range bytes.Split(normalized, []byte("\n")) {
		if len(line) > 4096 {
			return Explanation{}, ErrFormat
		}
	}
	groups := bytes.Split(bytes.Trim(normalized, "\n"), []byte("\n\n"))
	// Each recipient may have one detached metadata group. Still never merge them.
	if len(groups) < 2 || len(groups) > 2*MaxRecipients+1 {
		return Explanation{}, ErrFormat
	}
	digest := sha256.Sum256(raw)
	out := Explanation{Version: "dsn-explanation-v1", SHA256: hex.EncodeToString(digest[:]), Trust: "UNVERIFIED", RawStatus: string(body), Blocks: []ExplainedBlock{}, Issues: []FormatIssue{}}
	_, strictErr := parseStatus(body)
	out.StrictlyParsed = strictErr == nil
	issue := func(i int, code, field string) {
		out.Issues = append(out.Issues, FormatIssue{Block: i, Code: code, Field: field})
	}
	for i, group := range groups {
		fields, e := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(bytes.Clone(group), '\n', '\n')))).ReadMIMEHeader()
		if e != nil || len(fields) == 0 {
			return Explanation{}, ErrFormat
		}
		block := ExplainedBlock{Index: i, Fields: fields}
		duplicate := false
		for name, values := range fields {
			if len(values) > 1 {
				duplicate = true
				issue(i, "DUPLICATE_FIELD", name)
			}
		}
		if i == 0 {
			if !typed(fields.Get("Reporting-MTA")) {
				issue(i, "INVALID_REPORTING_MTA", "Reporting-MTA")
			}
		} else if fields.Get("Final-Recipient") == "" {
			issue(i, "UNASSIGNED_BLOCK", "")
		} else {
			final := fields.Get("Final-Recipient")
			if !typed(final) {
				issue(i, "MISSING_ADDRESS_TYPE", "Final-Recipient")
				// Only an exact bare mailbox is a hint; never infer from display names,
				// diagnostics, message headers, or detached groups.
				if !duplicate {
					if a, e := mail.ParseAddress(final); e == nil && a.Name == "" && a.Address == final && strings.Count(final, "@") == 1 && !strings.ContainsAny(final, " <>\t") {
						block.Hints.Mailbox = final
					}
				}
			}
			status := fields.Get("Status")
			if !statusPattern.MatchString(status) {
				issue(i, "INVALID_ENHANCED_STATUS", "Status")
				if !duplicate && len(status) == 3 {
					if code, e := strconv.Atoi(status); e == nil && code >= 400 && code <= 599 {
						block.Hints.SMTPCode = code
						issue(i, "SMTP_CODE_IN_STATUS", "Status")
					}
				}
			}
			action := strings.ToLower(fields.Get("Action"))
			switch action {
			case "failed", "delayed", "delivered", "relayed", "expanded":
			default:
				issue(i, "INVALID_ACTION", "Action")
			}
		}
		out.Blocks = append(out.Blocks, block)
	}
	if !out.StrictlyParsed {
		issue(-1, "STRICT_PARSE_REJECTED", "")
	}
	sort.Slice(out.Issues, func(i, j int) bool {
		a, b := out.Issues[i], out.Issues[j]
		if a.Block != b.Block {
			return a.Block < b.Block
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Code < b.Code
	})
	return out, nil
}

// ExplainStored reads immutable evidence without rewriting old parsed JSON,
// adding reviews, or enrolling compatibility hints in the decision engine.
func (s Service) ExplainStored(ctx context.Context, id, expected string) (Explanation, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Explanation{}, errors.New("valid feedback UUID required")
	}
	digest, err := hex.DecodeString(expected)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != expected {
		return Explanation{}, errors.New("canonical evidence SHA-256 required")
	}
	var raw []byte
	var stored string
	if err = s.DB.QueryRowContext(ctx, `SELECT raw,sha256 FROM feedback_reports WHERE id=$1`, id).Scan(&raw, &stored); err != nil {
		return Explanation{}, err
	}
	actual := sha256.Sum256(raw)
	if expected != stored || expected != hex.EncodeToString(actual[:]) {
		return Explanation{}, errors.New("evidence digest mismatch")
	}
	return Explain(raw)
}
