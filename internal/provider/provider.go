package provider

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"relaytale/internal/encryption"
)

type Provider struct {
	ID, Name, Host, Security, Username string
	Port, Priority, MaxConnections     int
	HourlyLimit, DailyLimit            int
	Timeout                            time.Duration
	Ciphertext, Nonce                  []byte
}

func Create(ctx context.Context, db *sql.DB, box *encryption.Box, p Provider, password string, domains []string) (string, error) {
	if p.Name == "" || p.Host == "" || strings.ContainsAny(p.Host, "\r\n /@") || p.Port < 1 || p.Port > 65535 || p.Username == "" || strings.ContainsAny(p.Username, "\r\n\x00") || password == "" || len(password) > 4096 {
		return "", errors.New("invalid provider settings")
	}
	if p.Security != "starttls" && p.Security != "implicit_tls" {
		return "", errors.New("provider requires starttls or implicit_tls")
	}
	if p.Timeout < time.Second || p.Timeout > 5*time.Minute || p.MaxConnections < 1 || p.MaxConnections > 32 {
		return "", errors.New("provider timeout must be 1s..5m and connections 1..32")
	}
	if p.HourlyLimit < 0 || p.DailyLimit < 0 || p.HourlyLimit > 2147483647 || p.DailyLimit > 2147483647 {
		return "", errors.New("provider limits must be 0 (unlimited) or positive 32-bit integers")
	}
	if len(domains) == 0 {
		return "", errors.New("at least one sender domain is required")
	}
	for i, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || strings.ContainsAny(d, "\r\n @/:*") || net.ParseIP(d) != nil {
			return "", errors.New("invalid sender domain")
		}
		domains[i] = d
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	p.ID = id.String()
	ciphertext, nonce, err := box.Seal(p.ID, password)
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO providers(id,name,enabled,host,port,security,username,password_ciphertext,nonce,priority,max_connections,timeout_seconds,from_domains,hourly_limit,daily_limit)
 VALUES($1,$2,true,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NULLIF($13,0),NULLIF($14,0))`, p.ID, p.Name, p.Host, p.Port, p.Security, p.Username, ciphertext, nonce, p.Priority, p.MaxConnections, int(p.Timeout/time.Second), domains, p.HourlyLimit, p.DailyLimit)
	if err != nil {
		return "", errors.New("cannot create provider; check name uniqueness and database availability")
	}
	return p.ID, nil
}
