package smtpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	"mailgateway/internal/provider"
)

type Client struct {
	Domain  string
	RootCAs *x509.CertPool
}

// Send uses a single bounded SMTP conversation. beforeData is a durable fence:
// no DATA command or body may be transmitted unless it commits successfully.
func (c Client) Send(ctx context.Context, p provider.Provider, password, from string, recipients []Recipient, payload []byte, beforeData func(context.Context) error) (out Result) {
	out.StartedAt = time.Now().UTC()
	for _, r := range recipients {
		out.Recipients = append(out.Recipients, RecipientResult{ID: r.ID, Status: Temporary})
	}
	defer func() { out.FinishedAt = time.Now().UTC(); out.Aggregate() }()
	stage := "CONNECT_ERROR"
	fail := func(err error, uncertain bool) {
		out.ErrorClass = stage
		out.ErrorMessage = "SMTP operation failed"
		status := Temporary
		var reply *textproto.Error
		if errors.As(err, &reply) {
			out.Code = reply.Code
			out.Response = safeResponse(reply.Msg, password)
			if reply.Code >= 500 && reply.Code <= 599 {
				status = Permanent
			} else if uncertain && (reply.Code < 400 || reply.Code > 599) {
				status = Unknown
			}
			out.Enhanced = enhanced(reply.Msg)
		} else if uncertain {
			status = Unknown
			out.ErrorClass = "FINAL_RESPONSE_UNKNOWN"
		}
		if stage == "AUTH_ERROR" {
			out.Response = "authentication rejected (redacted)"
		}
		for i := range out.Recipients {
			if out.Recipients[i].Status == "RCPT_ACCEPTED" || out.Recipients[i].Status == Temporary && out.Recipients[i].Code == 0 {
				out.Recipients[i].Status = status
				out.Recipients[i].Code = out.Code
				out.Recipients[i].Response = out.Response
				out.Recipients[i].Enhanced = out.Enhanced
			}
		}
	}
	event := func(kind, rid string, code int, response string) {
		out.Events = append(out.Events, Event{Type: kind, RecipientID: rid, At: time.Now().UTC(), Code: code, Response: safeResponse(response, password)})
	}
	if p.Security != "starttls" && p.Security != "implicit_tls" {
		stage = "TLS_ERROR"
		fail(errors.New("TLS required"), false)
		return
	}
	if strings.ContainsAny(from+c.Domain, "\r\n") {
		stage = "INVALID_ENVELOPE"
		fail(errors.New("invalid envelope"), false)
		return
	}
	for _, r := range recipients {
		if strings.ContainsAny(r.Address, "\r\n") {
			stage = "INVALID_ENVELOPE"
			fail(errors.New("invalid recipient"), false)
			return
		}
	}
	// Refuse a non-canonical snapshot rather than silently reserialize MIME.
	if !bytes.HasSuffix(payload, []byte("\r\n")) || bytes.Contains(bytes.ReplaceAll(payload, []byte("\r\n"), nil), []byte("\n")) {
		stage = "INVALID_EML"
		fail(errors.New("EML must use CRLF"), false)
		return
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	if err != nil {
		fail(err, false)
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	out.ConnectedAt = time.Now().UTC()
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	out.RemoteIP = host
	event("TCP_CONNECTED", "", 0, "")
	tlsConfig := &tls.Config{ServerName: p.Host, MinVersion: tls.VersionTLS12, RootCAs: c.RootCAs}
	var transport net.Conn = conn
	if p.Security == "implicit_tls" {
		stage = "TLS_ERROR"
		secure := tls.Client(conn, tlsConfig)
		if err := secure.HandshakeContext(ctx); err != nil {
			fail(err, false)
			return
		}
		transport = secure
		out.TLSAt = time.Now().UTC()
		event("TLS_ESTABLISHED", "", 0, "")
	}
	wire := textproto.NewConn(transport)
	defer func() { _ = wire.Close() }()
	stage = "SMTP_GREETING_ERROR"
	if _, _, err = wire.ReadResponse(220); err != nil {
		fail(err, false)
		return
	}
	command := func(expected int, format string, args ...any) (int, string, error) {
		if err := wire.PrintfLine(format, args...); err != nil {
			return 0, "", err
		}
		return wire.ReadResponse(expected)
	}
	domain := c.Domain
	if domain == "" {
		domain = "localhost"
	}
	stage = "EHLO_ERROR"
	_, caps, err := command(250, "EHLO %s", domain)
	if err != nil {
		fail(err, false)
		return
	}
	if p.Security == "starttls" {
		stage = "TLS_ERROR"
		if !hasCapability(caps, "STARTTLS") {
			fail(errors.New("STARTTLS missing"), false)
			return
		}
		if _, _, err = command(220, "STARTTLS"); err != nil {
			fail(err, false)
			return
		}
		secure := tls.Client(conn, tlsConfig)
		if err = secure.HandshakeContext(ctx); err != nil {
			fail(err, false)
			return
		}
		wire = textproto.NewConn(secure)
		out.TLSAt = time.Now().UTC()
		event("TLS_ESTABLISHED", "", 0, "")
		stage = "EHLO_ERROR"
		_, caps, err = command(250, "EHLO %s", domain)
		if err != nil {
			fail(err, false)
			return
		}
	}
	stage = "AUTH_ERROR"
	var auth sasl.Client
	mechanisms := capability(caps, "AUTH")
	for _, m := range strings.Fields(mechanisms) {
		if m == "PLAIN" {
			auth = sasl.NewPlainClient("", p.Username, password)
			break
		}
		if m == "LOGIN" {
			auth = sasl.NewLoginClient(p.Username, password)
		}
	}
	if auth == nil {
		fail(errors.New("no supported AUTH mechanism"), false)
		return
	}
	mech, initial, err := auth.Start()
	if err != nil {
		fail(err, false)
		return
	}
	code, challenge, err := command(0, "AUTH %s %s", mech, base64.StdEncoding.EncodeToString(initial))
	for i := 0; err == nil && code == 334 && i < 10; i++ {
		decoded, e := base64.StdEncoding.DecodeString(challenge)
		if e != nil {
			err = e
			break
		}
		response, e := auth.Next(decoded)
		if e != nil {
			err = e
			break
		}
		code, challenge, err = command(0, "%s", base64.StdEncoding.EncodeToString(response))
	}
	if err == nil && code != 235 {
		err = &textproto.Error{Code: code, Msg: "authentication rejected"}
	}
	if err != nil {
		fail(err, false)
		return
	}
	out.AuthenticatedAt = time.Now().UTC()
	event("AUTH_SUCCEEDED", "", 235, "")
	stage = "MAIL_FROM_REJECTED"
	if _, _, err = command(250, "MAIL FROM:<%s>", from); err != nil {
		fail(err, false)
		return
	}
	out.MailFromAt = time.Now().UTC()
	event("MAIL_FROM_ACCEPTED", "", 250, "")
	accepted := 0
	for i, r := range recipients {
		stage = "RCPT_REJECTED"
		code, response, err := command(25, "RCPT TO:<%s>", r.Address)
		if err != nil {
			var reply *textproto.Error
			if !errors.As(err, &reply) {
				fail(err, false)
				return
			}
			status := Temporary
			if code >= 500 {
				status = Permanent
			}
			out.Recipients[i] = RecipientResult{ID: r.ID, Status: status, Code: code, Enhanced: enhanced(response), Response: safeResponse(response, password)}
			event("RCPT_REJECTED", r.ID, code, response)
			continue
		}
		accepted++
		out.Recipients[i] = RecipientResult{ID: r.ID, Status: "RCPT_ACCEPTED", Code: code, Enhanced: enhanced(response), Response: safeResponse(response, password)}
		event("RCPT_ACCEPTED", r.ID, code, response)
	}
	out.RcptAt = time.Now().UTC()
	if accepted == 0 {
		return
	}
	stage = "DATABASE_ERROR"
	if err := beforeData(ctx); err != nil {
		fail(err, false)
		return
	}
	stage = "DATA_REJECTED"
	if _, _, err = command(354, "DATA"); err != nil {
		fail(err, false)
		return
	}
	out.DataStartedAt = time.Now().UTC()
	event("DATA_STARTED", "", 354, "")
	stage = "DATA_WRITE_ERROR"
	// Dot-stuff only; payload bytes (including CRLF and headers) stay unchanged.
	stuffed := dotStuff(payload)
	if _, err = io.Copy(wire.W, bytes.NewReader(stuffed)); err != nil {
		fail(err, true)
		return
	}
	if err = wire.W.Flush(); err != nil {
		fail(err, true)
		return
	}
	out.BytesSent = int64(len(payload))
	out.DataCompletedAt = time.Now().UTC()
	event("DATA_COMPLETED", "", 0, "")
	stage = "FINAL_RESPONSE_ERROR"
	code, response, err := wire.ReadResponse(2)
	if code != 0 {
		out.FinalResponseAt = time.Now().UTC()
	}
	if err != nil {
		fail(err, true)
		return
	}
	out.FinalResponseAt = time.Now().UTC()
	out.Code = code
	out.Response = safeResponse(response, password)
	out.Enhanced = enhanced(response)
	for i := range out.Recipients {
		if out.Recipients[i].Status == "RCPT_ACCEPTED" {
			out.Recipients[i].Status = Accepted
		}
	}
	event("SMTP_ACCEPTED", "", code, response)
	// No QUIT reply can change a known final acceptance.
	_ = wire.PrintfLine("QUIT")
	return
}
func capability(caps, name string) string {
	for _, line := range strings.Split(caps, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.EqualFold(f[0], name) {
			return strings.Join(f[1:], " ")
		}
	}
	return ""
}
func hasCapability(caps, name string) bool {
	for _, line := range strings.Split(caps, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), name) {
			return true
		}
	}
	return false
}
func enhanced(response string) string {
	f := strings.Fields(response)
	if len(f) > 0 {
		var a, b, c int
		if n, _ := fmt.Sscanf(f[0], "%d.%d.%d", &a, &b, &c); n == 3 {
			return f[0]
		}
	}
	return ""
}
func safeResponse(s, password string) string {
	if password != "" {
		s = strings.ReplaceAll(s, password, "[REDACTED]")
		s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(password)), "[REDACTED]")
	}
	if len(s) > 4096 {
		s = s[:4096]
	}
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
}
func dotStuff(raw []byte) []byte {
	var b bytes.Buffer
	lineStart := true
	for _, v := range raw {
		if lineStart && v == '.' {
			b.WriteByte('.')
		}
		b.WriteByte(v)
		lineStart = v == '\n'
	}
	b.WriteString(".\r\n")
	return b.Bytes()
}
