package smtpserver

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"mailgateway/internal/auth"
	"mailgateway/internal/message"
	"mailgateway/internal/storage"
)

type Authenticator interface {
	Authenticate(context.Context, string, string) (auth.Account, error)
}
type Receiver interface {
	Receive(context.Context, message.Envelope, io.Reader) (string, error)
}
type Backend struct {
	Accounts Authenticator
	Receiver Receiver
	Log      *slog.Logger
	Timeout  time.Duration
}

func New(b *Backend, domain string, cert tls.Certificate, maxBytes int64) *Server {
	s := smtp.NewServer(b)
	s.Domain = domain
	s.MaxRecipients = 100
	s.MaxMessageBytes = maxBytes
	s.ReadTimeout = 60 * time.Second
	s.WriteTimeout = 60 * time.Second
	s.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	s.ErrorLog = safeLogger{b.Log}
	return &Server{Server: s, connections: make(map[net.Conn]struct{})}
}

// Library errors can include untrusted protocol data. No raw AUTH transcript
// or panic values are forwarded to structured application logs.
type safeLogger struct{ log *slog.Logger }

func (l safeLogger) Printf(string, ...interface{}) {
	l.log.Warn("SMTP connection error", "component", "smtp")
}
func (l safeLogger) Println(...interface{}) { l.log.Warn("SMTP connection error", "component", "smtp") }
func (b *Backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{backend: b, conn: c}, nil
}

type session struct {
	backend      *Backend
	conn         *smtp.Conn
	account      *auth.Account
	from         string
	recipients   []string
	authAttempts int
}

func (s *session) Reset()                   { s.from = ""; s.recipients = nil }
func (s *session) Logout() error            { s.Reset(); s.account = nil; return nil }
func (s *session) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }
func (s *session) Auth(mech string) (sasl.Server, error) {
	if _, ok := s.conn.TLSConnectionState(); !ok {
		return nil, &smtp.SMTPError{Code: 538, EnhancedCode: smtp.EnhancedCode{5, 7, 11}, Message: "STARTTLS required"}
	}
	if s.authAttempts >= 5 {
		return nil, &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many authentication attempts"}
	}
	s.authAttempts++
	switch mech {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if identity != "" && identity != username {
				return smtp.ErrAuthFailed
			}
			return s.authenticate(username, password)
		}), nil
	case sasl.Login:
		return &login{authenticate: s.authenticate}, nil
	default:
		return nil, smtp.ErrAuthUnknownMechanism
	}
}
func (s *session) authenticate(username, password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := s.backend.Accounts.Authenticate(ctx, username, password)
	if errors.Is(err, auth.ErrCredentials) {
		return smtp.ErrAuthFailed
	}
	if err != nil {
		return &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Authentication temporarily unavailable"}
	}
	s.account = &a
	return nil
}
func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	if s.account == nil {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	}
	s.Reset()
	if !s.account.Allows(from) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Sender not allowed"}
	}
	s.from = from
	return nil
}
func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if s.account == nil || s.from == "" {
		return smtp.ErrAuthRequired
	}
	canonical, err := auth.CanonicalAddress(to)
	if err != nil {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Invalid recipient"}
	}
	for _, prior := range s.recipients {
		if prior == canonical {
			return nil
		}
	}
	s.recipients = append(s.recipients, canonical)
	return nil
}
func (s *session) Data(r io.Reader) error {
	if s.account == nil || s.from == "" || len(s.recipients) == 0 {
		return smtp.ErrAuthRequired
	}
	defer s.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), s.backend.Timeout)
	defer cancel()
	id, err := s.backend.Receiver.Receive(ctx, message.Envelope{Account: *s.account, From: s.from, Recipients: append([]string(nil), s.recipients...)}, r)
	if errors.Is(err, message.ErrSenderDenied) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Header sender not allowed"}
	}
	if errors.Is(err, message.ErrInvalidMessage) || errors.Is(err, smtp.ErrTooLongLine) {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 6, 0}, Message: "Invalid message headers"}
	}
	if errors.Is(err, storage.ErrTooLarge) || errors.Is(err, smtp.ErrDataTooLarge) {
		return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 3, 4}, Message: "Message too large"}
	}
	if err != nil {
		s.backend.Log.Error("message persistence failed", "component", "smtp")
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Unable to durably queue message; retry later"}
	}
	s.backend.Log.Info("message queued", "component", "smtp", "message_id", id)
	return nil
}

// LOGIN exists for legacy submission clients; PLAIN over TLS is preferred.
type login struct {
	step         int
	username     string
	authenticate func(string, string) error
}

func (l *login) Next(response []byte) ([]byte, bool, error) {
	switch l.step {
	case 0:
		l.step = 1
		if response == nil {
			return []byte("Username:"), false, nil
		}
		fallthrough
	case 1:
		if len(response) == 0 || len(response) > 128 {
			return nil, true, smtp.ErrAuthFailed
		}
		l.username = string(response)
		l.step = 2
		return []byte("Password:"), false, nil
	case 2:
		l.step = 3
		return nil, true, l.authenticate(l.username, string(response))
	default:
		return nil, true, smtp.ErrAuthFailed
	}
}

// go-smtp Close returns early after Shutdown has begun. Track transports so a
// shutdown deadline can still forcibly interrupt stalled DATA uploads.
type Server struct {
	*smtp.Server
	mu          sync.Mutex
	connections map[net.Conn]struct{}
}

func (s *Server) Serve(l net.Listener) error {
	return s.Server.Serve(&trackingListener{Listener: l, server: s})
}
func (s *Server) Close() error {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.connections))
	for c := range s.connections {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	err := s.Server.Close()
	if errors.Is(err, smtp.ErrServerClosed) {
		return nil
	}
	return err
}

type trackingListener struct {
	net.Listener
	server *Server
}

func (l *trackingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.server.mu.Lock()
	l.server.connections[c] = struct{}{}
	l.server.mu.Unlock()
	return &trackedConn{Conn: c, server: l.server}, nil
}

type trackedConn struct {
	net.Conn
	server *Server
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.server.mu.Lock()
	delete(c.server.connections, c.Conn)
	c.server.mu.Unlock()
	return err
}
