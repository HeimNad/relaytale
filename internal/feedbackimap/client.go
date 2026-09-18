// Package feedbackimap reads a configured mailbox without changing its flags.
package feedbackimap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"relaytale/internal/feedback"
)

const wireLimit = 16 << 20

var errProtocol = errors.New("invalid or excessive IMAP response")

type Config struct {
	Host                       string
	Port                       int
	Username, Password, Folder string
}

func (v Config) Validate() error {
	if v.Port < 1 || v.Port > 65535 || v.Host == "" || len(v.Host) > 253 || strings.ContainsAny(v.Host, " /\\\r\n\x00") {
		return errors.New("invalid IMAP host or port")
	}
	for _, s := range []string{v.Username, v.Password, v.Folder} {
		if s == "" || len(s) > 1024 || strings.ContainsAny(s, "\r\n\x00") {
			return errors.New("IMAP username, password and explicit folder are required (maximum 1024 bytes)")
		}
	}
	return nil
}
func (v Config) Identity() string {
	b, _ := json.Marshal([]string{strings.ToLower(v.Host), strconv.Itoa(v.Port), v.Username, v.Folder})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Client struct {
	c      *imapclient.Client
	conn   net.Conn
	folder string
	stop   func() bool
}

// Dial uses implicit TLS with certificate verification. No plaintext fallback.
func Dial(ctx context.Context, v Config) (feedback.Mailbox, error) { return dial(ctx, v, nil) }
func dial(ctx context.Context, v Config, roots *x509.CertPool) (feedback.Mailbox, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(v.Host, strconv.Itoa(v.Port)))
	if err != nil {
		return nil, errors.New("IMAP dial failed")
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	secure := tls.Client(conn, &tls.Config{ServerName: v.Host, MinVersion: tls.VersionTLS12, RootCAs: roots})
	handshake, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = secure.HandshakeContext(handshake); err != nil {
		stop()
		_ = conn.Close()
		return nil, errors.New("IMAP TLS verification failed")
	}
	bounded := &boundedConn{Conn: secure}
	bounded.remaining.Store(wireLimit)
	c := imapclient.New(bounded, nil)
	client := &Client{c: c, conn: bounded, folder: v.Folder, stop: stop}
	if err = c.Login(v.Username, v.Password).Wait(); err != nil {
		_ = client.Close()
		return nil, errors.New("IMAP authentication failed")
	}
	return client, nil
}
func (c *Client) Close() error { c.stop(); return c.conn.Close() }
func (c *Client) Select(ctx context.Context) (feedback.MailboxSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return feedback.MailboxSnapshot{}, err
	}
	data, err := c.c.Select(c.folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return feedback.MailboxSnapshot{}, errProtocol
	}
	return feedback.MailboxSnapshot{UIDValidity: data.UIDValidity, UIDNext: uint32(data.UIDNext)}, nil
}
func (c *Client) Metadata(ctx context.Context, first, last uint32) (out []feedback.MailMetadata, err error) {
	if first == 0 || last < first || uint64(last)-uint64(first) >= 100 {
		return nil, errProtocol
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(first), imap.UID(last))
	cmd := c.c.Fetch(set, &imap.FetchOptions{UID: true, RFC822Size: true})
	defer func() {
		if err != nil {
			_ = c.Close()
		}
		if e := cmd.Close(); err == nil && e != nil {
			err = errProtocol
		}
	}()
	for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
		if len(out) >= 100 {
			return nil, errProtocol
		}
		m := feedback.MailMetadata{}
		haveUID, haveSize := false, false
		for item := msg.Next(); item != nil; item = msg.Next() {
			switch v := item.(type) {
			case imapclient.FetchItemDataUID:
				if haveUID {
					return nil, errProtocol
				}
				haveUID = true
				m.UID = uint32(v.UID)
			case imapclient.FetchItemDataRFC822Size:
				if haveSize {
					return nil, errProtocol
				}
				haveSize = true
				m.Size = v.Size
			default:
				return nil, errProtocol
			}
		}
		if !haveUID || !haveSize || m.UID < first || m.UID > last || m.Size < 0 {
			return nil, errProtocol
		}
		out = append(out, m)
	}
	return out, nil
}
func (c *Client) Body(ctx context.Context, uid uint32) (raw []byte, present bool, err error) {
	if uid == 0 {
		return nil, false, errProtocol
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Size: feedback.MaxBytes + 1}}
	cmd := c.c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}})
	defer func() {
		if err != nil {
			_ = c.Close()
		}
		if e := cmd.Close(); err == nil && e != nil {
			err = errProtocol
		}
	}()
	count := 0
	for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
		count++
		if count > 1 {
			return nil, false, errProtocol
		}
		haveUID, haveBody := false, false
		for item := msg.Next(); item != nil; item = msg.Next() {
			switch v := item.(type) {
			case imapclient.FetchItemDataUID:
				if haveUID || uint32(v.UID) != uid {
					return nil, false, errProtocol
				}
				haveUID = true
			case imapclient.FetchItemDataBodySection:
				if haveBody || !v.MatchCommand(section) {
					return nil, false, errProtocol
				}
				haveBody = true
				if v.Literal != nil {
					if v.Literal.Size() < 0 || v.Literal.Size() > feedback.MaxBytes+1 {
						return nil, false, errProtocol
					}
					raw, err = io.ReadAll(io.LimitReader(v.Literal, feedback.MaxBytes+1))
					if err != nil {
						return nil, false, errProtocol
					}
					present = true
				}
			default:
				return nil, false, errProtocol
			}
		}
		if !haveUID || !haveBody {
			return nil, false, errProtocol
		}
	}
	return raw, present, nil
}

// The library decoder also reads unrequested server data. Bound the entire
// session, not merely BODY literals, before it reaches that decoder.
type boundedConn struct {
	net.Conn
	remaining atomic.Int64
}

func (c *boundedConn) Read(p []byte) (int, error) {
	left := c.remaining.Load()
	if left <= 0 {
		_ = c.Conn.Close()
		return 0, errProtocol
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	n, err := c.Conn.Read(p)
	c.remaining.Add(-int64(n))
	return n, err
}
