package smtpclient

import (
	"strconv"
	"strings"
)

// Capabilities contains only allowlisted post-TLS EHLO observations, not raw
// server text or proof of message acceptance. Nil means not observed securely.
type Capabilities struct {
	SMTPUTF8     bool   `json:"smtputf8_advertised"`
	EightBitMIME bool   `json:"eightbitmime_advertised"`
	Pipelining   bool   `json:"pipelining_advertised"`
	AuthPlain    bool   `json:"auth_plain_advertised"`
	AuthLogin    bool   `json:"auth_login_advertised"`
	SizeState    string `json:"size_state"`
	SizeBytes    *int64 `json:"size_bytes,omitempty"`
}

func observedCapabilities(raw string) *Capabilities {
	c := &Capabilities{SizeState: "absent"}
	sizeSeen := false
	// The first EHLO line is server identification, never an extension.
	lines := strings.Split(raw, "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "SMTPUTF8":
			c.SMTPUTF8 = true
		case "8BITMIME":
			c.EightBitMIME = true
		case "PIPELINING":
			c.Pipelining = true
		case "AUTH":
			for _, m := range fields[1:] {
				switch strings.ToUpper(m) {
				case "PLAIN":
					c.AuthPlain = true
				case "LOGIN":
					c.AuthLogin = true
				}
			}
		case "SIZE":
			if sizeSeen {
				c.SizeState = "invalid"
				c.SizeBytes = nil
				continue
			}
			sizeSeen = true
			if len(fields) == 1 {
				c.SizeState = "unspecified"
				continue
			}
			n, err := strconv.ParseInt(fields[1], 10, 64)
			if len(fields) != 2 || err != nil || n < 0 || strings.HasPrefix(fields[1], "+") {
				c.SizeState = "invalid"
				continue
			}
			c.SizeState = "numeric"
			c.SizeBytes = &n
		}
	}
	return c
}
