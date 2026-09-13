package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
)

var ErrCredentials = errors.New("invalid credentials")
var ErrBusy = errors.New("authentication busy")

type Account struct {
	ID          string
	AllowedFrom []string
}

// CanonicalAddress permits bare ASCII mailbox addresses only. Domain case is
// ignored while local-part case remains significant.
func CanonicalAddress(value string) (string, error) {
	if len(value) > 254 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid address")
	}
	for _, r := range value {
		if r > 127 {
			return "", errors.New("SMTPUTF8 is not enabled")
		}
	}
	a, err := mail.ParseAddress(value)
	if err != nil || a.Name != "" || a.Address != value {
		return "", errors.New("expected bare mailbox address")
	}
	at := strings.LastIndexByte(value, '@')
	if at <= 0 || at == len(value)-1 {
		return "", errors.New("invalid address")
	}
	return value[:at+1] + strings.ToLower(value[at+1:]), nil
}
func (a Account) Allows(from string) bool {
	value, err := CanonicalAddress(from)
	if err != nil {
		return false
	}
	for _, allowed := range a.AllowedFrom {
		v, err := CanonicalAddress(allowed)
		if err == nil && v == value {
			return true
		}
	}
	return false
}

type Accounts struct {
	DB    *sql.DB
	slots chan struct{}
	dummy string
}

func NewAccounts(db *sql.DB) (*Accounts, error) { return NewAccountsWithConcurrency(db, 2) }
func NewAccountsWithConcurrency(db *sql.DB, concurrency int) (*Accounts, error) {
	if concurrency < 1 || concurrency > 8 {
		return nil, errors.New("authentication concurrency must be 1..8")
	}

	dummy, err := Hash("unusable-dummy-password-for-timing")
	if err != nil {
		return nil, err
	}
	return &Accounts{DB: db, slots: make(chan struct{}, concurrency), dummy: dummy}, nil
}
func (a *Accounts) Authenticate(ctx context.Context, username, password string) (Account, error) {
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		return Account{}, ErrBusy
	}
	if len(username) > 128 || len(password) > 1024 {
		return Account{}, ErrCredentials
	}
	var account Account
	var hash string
	var allowed []byte
	var enabled bool
	err := a.DB.QueryRowContext(ctx, `SELECT id,password_hash,to_json(allowed_from),enabled FROM smtp_accounts WHERE username=$1`, username).Scan(&account.ID, &hash, &allowed, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		_ = Verify(a.dummy, password)
		return Account{}, ErrCredentials
	}
	if err != nil {
		return Account{}, err
	}
	valid := Verify(hash, password)
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	if !valid || !enabled {
		return Account{}, ErrCredentials
	}
	if err := json.Unmarshal(allowed, &account.AllowedFrom); err != nil {
		return Account{}, err
	}
	return account, nil
}
