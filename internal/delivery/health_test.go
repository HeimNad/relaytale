package delivery

import "testing"

func TestProviderHealthAttribution(t *testing.T) {
	for _, tc := range []struct {
		e    Evidence
		want string
	}{
		{Evidence{Status: "SMTP_ACCEPTED", FinalResponse: true, FinalCode: 250}, "SUCCESS"},
		{Evidence{Status: "PARTIAL_ACCEPTED", FinalResponse: true, FinalCode: 250}, "SUCCESS"},
		{Evidence{Stage: "RCPT_REJECTED", Code: 450}, "IGNORED"},
		{Evidence{Stage: "RCPT_REJECTED", Code: 550}, "IGNORED"},
		{Evidence{ErrorClass: "AUTH_ERROR", Code: 535}, "IGNORED"},
		{Evidence{ErrorClass: "AUTH_ERROR", Code: 454}, "FAILURE"},
		{Evidence{ErrorClass: "LOCAL_STORAGE_ERROR"}, "IGNORED"},
		{Evidence{ErrorClass: "CREDENTIAL_DECRYPTION_ERROR"}, "IGNORED"},
		{Evidence{ErrorClass: "DNS_ERROR"}, "FAILURE"},
		{Evidence{ErrorClass: "TLS_ERROR"}, "FAILURE"},
		{Evidence{Stage: "FINAL_RESPONSE_ERROR", Status: "DELIVERY_UNKNOWN", ErrorClass: "FINAL_RESPONSE_TIMEOUT"}, "FAILURE"},
		{Evidence{Stage: "FINAL_RESPONSE_ERROR", Code: 451}, "FAILURE"},
		{Evidence{Stage: "FINAL_RESPONSE_ERROR", Code: 550}, "IGNORED"},
	} {
		if got := ProviderOutcome(tc.e); got != tc.want {
			t.Fatalf("%+v: %s want %s", tc.e, got, tc.want)
		}
	}
}
