package smtpclient

import "time"

const (
	Accepted  = "SMTP_ACCEPTED"
	Temporary = "TEMP_FAILED"
	Permanent = "PERM_FAILED"
	Unknown   = "DELIVERY_UNKNOWN"
	Partial   = "PARTIAL_ACCEPTED"
)

type Recipient struct{ ID, Address string }
type RecipientResult struct {
	ID, Status, Stage  string
	Code               int
	Enhanced, Response string
}
type Event struct {
	Sequence          int
	Type, RecipientID string
	At                time.Time
	Code              int
	Response          string
}
type Result struct {
	Stage                                                                                                                           string
	DNSStartedAt, DNSCompletedAt                                                                                                    time.Time
	Timings                                                                                                                         map[string]int64
	RecorderError                                                                                                                   bool
	Status, ErrorClass, ErrorMessage                                                                                                string
	Code                                                                                                                            int
	Enhanced, Response, RemoteIP                                                                                                    string
	StartedAt, ConnectedAt, TLSAt, AuthenticatedAt, MailFromAt, RcptAt, DataStartedAt, DataCompletedAt, FinalResponseAt, FinishedAt time.Time
	BytesSent                                                                                                                       int64
	Recipients                                                                                                                      []RecipientResult
	Events                                                                                                                          []Event
}

func (r *Result) Aggregate() {
	accepted, temporary, unknown := 0, 0, 0
	for _, v := range r.Recipients {
		switch v.Status {
		case Accepted:
			accepted++
		case Temporary:
			temporary++
		case Unknown:
			unknown++
		}
	}
	switch {
	case unknown > 0:
		r.Status = Unknown
	case accepted == len(r.Recipients) && accepted > 0:
		r.Status = Accepted
	case accepted > 0:
		r.Status = Partial
	case temporary > 0:
		r.Status = Temporary
	default:
		r.Status = Permanent
	}
}
