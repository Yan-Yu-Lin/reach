package main

import "testing"

// Daybreak P2-10 / astra P2-11: the agent binary validates the API URL
// itself; prefix tricks and plain http to remote hosts are refused.
func TestValidateAPIURL(t *testing.T) {
	for _, ok := range []string{"https://tunnels.arthurlin.dev", "https://h.example:8443/x", "http://127.0.0.1:9300", "http://localhost", "http://[::1]:9300", "https://10.0.0.5"} {
		if err := validateAPIURL(ok); err != nil {
			t.Fatalf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://localhost.attacker.invalid", "http://127.0.0.1.attacker.invalid", "http://localhost@evil.example", "https://good@evil.example", "http://remote.invalid", "ftp://x", "https://h.example?x=1", "https://", "http://10.0.0.5", "https://h.example:99999"} {
		if err := validateAPIURL(bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}
