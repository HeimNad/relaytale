// Package testsmtp is a local SMTP provider fixture for protocol/failure tests.
package testsmtp

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"relaytale/internal/devtls"
)

type Options struct {
	// Load fixtures hash DATA without retaining a message-sized in-memory copy.
	DiscardPayload bool
	MaxBytes       int64
	ReadDelay      time.Duration // once per 64 KiB
	DropAfterBytes int64
	OnMessage      func(int64, string)

	RecipientCode                                  func(int, string) int
	LoginOnly                                      bool
	ImplicitTLS, NoSTARTTLS, RejectAuth, DropFinal bool
	FinalCode, DataCode                            int
	RejectRecipients                               map[string]int
	FinalDelay                                     time.Duration
}
type Server struct {
	Addr        string
	Roots       *x509.CertPool
	Payloads    chan []byte
	Envelopes   chan []string
	Connections atomic.Int32
	mu          sync.Mutex
	conns       map[net.Conn]bool
	listener    net.Listener
	wg          sync.WaitGroup
}

func Start(t *testing.T, o Options) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := devtls.Generate(dir); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "smtp.crt"), filepath.Join(dir, "smtp.key"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "smtp.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(raw)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Addr: listener.Addr().String(), Roots: roots, Payloads: make(chan []byte, 100), Envelopes: make(chan []string, 100), conns: map[net.Conn]bool{}, listener: listener}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			number := int(s.Connections.Add(1))
			s.mu.Lock()
			s.conns[c] = true
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer c.Close()
				defer func() { s.mu.Lock(); delete(s.conns, c); s.mu.Unlock() }()
				s.serve(c, cert, o, number)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}
func (s *Server) serve(raw net.Conn, cert tls.Certificate, o Options, number int) {
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	var transport net.Conn = raw
	secure := o.ImplicitTLS
	if secure {
		transport = tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	wire := textproto.NewConn(transport)
	defer func() { wire.Close() }()
	_ = wire.PrintfLine("220 localhost fake-provider")
	authenticated := false
	accepted := 0
	var addresses []string
	for {
		line, err := wire.ReadLine()
		if err != nil {
			return
		}
		command, argument, _ := strings.Cut(line, " ")
		switch strings.ToUpper(command) {
		case "EHLO":
			if secure {
				if o.LoginOnly {
					_ = wire.PrintfLine("250-localhost\r\n250-AUTH LOGIN\r\n250 8BITMIME")
				} else {
					_ = wire.PrintfLine("250-localhost\r\n250-AUTH PLAIN LOGIN\r\n250 8BITMIME")
				}
			} else if o.NoSTARTTLS {
				_ = wire.PrintfLine("250 localhost")
			} else {
				_ = wire.PrintfLine("250-localhost\r\n250 STARTTLS")
			}
		case "STARTTLS":
			if secure || o.NoSTARTTLS {
				_ = wire.PrintfLine("502 unavailable")
				continue
			}
			_ = wire.PrintfLine("220 ready for TLS")
			tlsConn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			wire = textproto.NewConn(tlsConn)
			secure = true
		case "AUTH":
			if !secure {
				_ = wire.PrintfLine("538 encryption required")
				continue
			}
			parts := strings.Fields(argument)
			valid := false
			if len(parts) == 2 && parts[0] == "PLAIN" {
				decoded, _ := base64.StdEncoding.DecodeString(parts[1])
				valid = string(decoded) == "\x00provider-user\x00provider-password"
			}
			if len(parts) > 0 && parts[0] == "LOGIN" {
				u := ""
				if len(parts) == 2 {
					u = parts[1]
				} else {
					_ = wire.PrintfLine("334 VXNlcm5hbWU6")
					var e error
					u, e = wire.ReadLine()
					if e != nil {
						return
					}
				}
				decodedUser, _ := base64.StdEncoding.DecodeString(u)
				_ = wire.PrintfLine("334 UGFzc3dvcmQ6")
				pw, e := wire.ReadLine()
				if e != nil {
					return
				}
				decodedPassword, _ := base64.StdEncoding.DecodeString(pw)
				valid = string(decodedUser) == "provider-user" && string(decodedPassword) == "provider-password"
			}
			if o.RejectAuth || !valid {
				_ = wire.PrintfLine("535 5.7.8 authentication rejected")
			} else {
				authenticated = true
				_ = wire.PrintfLine("235 2.7.0 authenticated")
			}
		case "MAIL":
			if !authenticated {
				_ = wire.PrintfLine("530 authenticate first")
				continue
			}
			accepted = 0
			addresses = nil
			_ = wire.PrintfLine("250 2.1.0 sender accepted")
		case "RCPT":
			address := strings.TrimSuffix(strings.TrimPrefix(argument, "TO:<"), ">")
			code := o.RejectRecipients[address]
			if o.RecipientCode != nil {
				code = o.RecipientCode(number, address)
			}
			if code != 0 {
				_ = wire.PrintfLine("%d 5.1.1 recipient rejected", code)
			} else {
				accepted++
				addresses = append(addresses, address)
				_ = wire.PrintfLine("250 2.1.5 recipient accepted")
			}
		case "DATA":
			if o.DataCode != 0 {
				_ = wire.PrintfLine("%d 4.3.0 DATA rejected", o.DataCode)
				continue
			}
			if accepted == 0 {
				_ = wire.PrintfLine("554 no recipients")
				continue
			}
			_ = wire.PrintfLine("354 send payload")
			var data bytes.Buffer
			hash := sha256.New()
			var size, delayed int64
			maxBytes := o.MaxBytes
			if maxBytes == 0 {
				maxBytes = 4 * 1024 * 1024
			}
			for {
				line, err := wire.R.ReadString('\n')
				if err != nil {
					return
				}
				if line == ".\r\n" {
					break
				}
				if strings.HasPrefix(line, "..") {
					line = line[1:]
				}
				size += int64(len(line))
				_, _ = hash.Write([]byte(line))
				if !o.DiscardPayload {
					data.WriteString(line)
				}
				if o.DropAfterBytes > 0 && size >= o.DropAfterBytes {
					return
				}
				if o.ReadDelay > 0 && size-delayed >= 64*1024 {
					time.Sleep(o.ReadDelay)
					delayed = size
				}
				if size > maxBytes {
					return
				}
			}
			if o.OnMessage != nil {
				o.OnMessage(size, hex.EncodeToString(hash.Sum(nil)))
			}
			if !o.DiscardPayload {
				s.Payloads <- append([]byte(nil), data.Bytes()...)
				s.Envelopes <- append([]string(nil), addresses...)
			}
			if o.DropFinal {
				return
			}
			if o.FinalDelay > 0 {
				timer := time.NewTimer(o.FinalDelay)
				<-timer.C
			}
			code := o.FinalCode
			if code == 0 {
				code = 250
			}
			_ = wire.PrintfLine("%d 2.0.0 provider-test-id", code)
		case "QUIT":
			_ = wire.PrintfLine("221 bye")
			return
		default:
			_ = wire.PrintfLine("502 unsupported")
		}
	}
}

// CloseListener simulates a provider becoming unreachable while preserving captured evidence.
func (s *Server) CloseListener() { _ = s.listener.Close() }
