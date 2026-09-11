package delivery

import "strings"

// ProviderOutcome is independent of delivery retry permission. A transport
// failure may hurt availability while the message itself must remain UNKNOWN.
// Local, configuration, recipient and permanent message rejections are ignored.
func ProviderOutcome(e Evidence) string {
	if e.FinalResponse && e.FinalCode >= 200 && e.FinalCode < 300 && (e.Status == "SMTP_ACCEPTED" || e.Status == "PARTIAL_ACCEPTED") {
		return "SUCCESS"
	}
	switch e.ErrorClass {
	case "LOCAL_STORAGE_ERROR", "DATABASE_ERROR", "RECORDER_ERROR", "INVALID_EML", "INVALID_ENVELOPE", "CREDENTIAL_DECRYPTION_ERROR":
		return "IGNORED"
	case "AUTH_ERROR":
		if e.Code >= 400 && e.Code < 500 {
			return "FAILURE"
		}
		return "IGNORED"
	}
	for _, prefix := range []string{"DNS_", "CONNECT_", "TLS_", "STARTTLS_", "SMTP_GREETING_", "EHLO_", "AUTH_CONNECTION_", "DATA_WRITE_", "FINAL_RESPONSE_UNKNOWN", "FINAL_RESPONSE_TIMEOUT"} {
		if strings.HasPrefix(e.ErrorClass, prefix) {
			return "FAILURE"
		}
	}
	if e.ErrorClass == "CONNECTION_RESET" {
		return "FAILURE"
	}
	if (e.Stage == "DATA_REJECTED" || e.Stage == "FINAL_RESPONSE_ERROR") && e.Code >= 400 && e.Code < 500 {
		return "FAILURE"
	}
	return "IGNORED"
}
