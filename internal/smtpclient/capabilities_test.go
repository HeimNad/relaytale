package smtpclient

import "testing"

func TestCapabilitiesUseOnlyExtensionsAndBoundedValues(t *testing.T) {
	c := observedCapabilities("SMTPUTF8\nAUTH PLAIN LOGIN OTHER\nSIZE 25000000\n8BITMIME\nPIPELINING")
	if c.SMTPUTF8 || !c.AuthPlain || !c.AuthLogin || !c.EightBitMIME || !c.Pipelining || c.SizeBytes == nil || *c.SizeBytes != 25000000 {
		t.Fatal("incorrect capability observation")
	}
	for _, raw := range []string{"server\nSIZE secret", "server\nSIZE -1", "server\nSIZE 1\nSIZE 2", "server\nSIZE 999999999999999999999"} {
		c = observedCapabilities(raw)
		if c.SizeState != "invalid" || c.SizeBytes != nil {
			t.Fatal("ambiguous SIZE accepted")
		}
	}
}
