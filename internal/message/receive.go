package message

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"mailgateway/internal/auth"
	"mailgateway/internal/storage"
)

var ErrInvalidMessage = errors.New("invalid message headers")
var ErrSenderDenied = errors.New("header sender not allowed")

type Envelope struct {
	Account    auth.Account
	From       string
	Recipients []string
}
type Submission struct {
	ID                             string
	Envelope                       Envelope
	Archive                        storage.Archive
	MessageID, HeaderFrom, Subject string
	ReceivedAt, ArchivedAt         time.Time
}
type ArchiveStore interface {
	Save(context.Context, string, time.Time, io.Reader) (storage.Archive, error)
}
type Repository interface {
	Enqueue(context.Context, Submission) error
}
type Receiver struct {
	Store ArchiveStore
	Repo  Repository
}

func (s Receiver) Receive(ctx context.Context, e Envelope, r io.Reader) (string, error) {
	if !e.Account.Allows(e.From) || len(e.Recipients) == 0 {
		return "", ErrSenderDenied
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	sub := Submission{ID: id.String(), Envelope: e, ReceivedAt: time.Now().UTC()}
	// Read only a bounded header, then stream its exact bytes and the remaining
	// DATA to storage. Never reserialize MIME or insert tracking headers.
	br := bufio.NewReader(r)
	header, err := readHeader(br)
	if err != nil {
		return "", err
	}
	sub.MessageID, sub.HeaderFrom, sub.Subject, err = metadata(header, e.Account)
	if err != nil {
		return "", err
	}
	sub.Archive, err = s.Store.Save(ctx, sub.ID, sub.ReceivedAt, io.MultiReader(bytes.NewReader(header), br))
	if err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	sub.ArchivedAt = time.Now().UTC()
	// Retain a published EML if commit fails: its outcome may be ambiguous.
	// An eventual orphan collector can reconcile it after a safety window.
	if err := s.Repo.Enqueue(ctx, sub); err != nil {
		return "", fmt.Errorf("enqueue: %w", err)
	}
	return sub.ID, nil
}

func readHeader(r *bufio.Reader) ([]byte, error) {
	var b bytes.Buffer
	lineBytes := 0
	for b.Len() <= 256*1024 {
		line, err := r.ReadSlice('\n')
		b.Write(line)
		lineBytes += len(line)
		if b.Len() > 256*1024 {
			return nil, ErrInvalidMessage
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ErrInvalidMessage
			}
			return nil, err
		}
		if (lineBytes == 2 && bytes.Equal(line, []byte("\r\n"))) || (lineBytes == 1 && bytes.Equal(line, []byte("\n"))) {
			return b.Bytes(), nil
		}
		lineBytes = 0
	}
	return nil, ErrInvalidMessage
}

func metadata(header []byte, account auth.Account) (string, string, string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(header))
	if err != nil {
		return "", "", "", ErrInvalidMessage
	}
	if len(msg.Header["From"]) != 1 {
		return "", "", "", ErrInvalidMessage
	}
	from, err := msg.Header.AddressList("From")
	if err != nil || len(from) != 1 {
		return "", "", "", ErrInvalidMessage
	}
	if !account.Allows(from[0].Address) {
		return "", "", "", ErrSenderDenied
	}
	if len(msg.Header["Sender"]) > 1 {
		return "", "", "", ErrInvalidMessage
	}
	if sender := msg.Header.Get("Sender"); sender != "" {
		a, err := mail.ParseAddress(sender)
		if err != nil || !account.Allows(a.Address) {
			return "", "", "", ErrSenderDenied
		}
	}
	return strings.TrimSpace(msg.Header.Get("Message-Id")), from[0].Address, msg.Header.Get("Subject"), nil
}
