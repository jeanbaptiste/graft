package activitypub

import "testing"

func TestValidateExternalURLRejectsUnsafeTargets(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"plain http", "http://example.social/users/alice"},
		{"loopback IP", "https://127.0.0.1/actors/x"},
		{"IPv6 loopback", "https://[::1]/actors/x"},
		{"localhost", "https://localhost/actors/x"},
		{"private range 10.x", "https://10.0.0.5/actors/x"},
		{"private range 192.168.x", "https://192.168.1.1/actors/x"},
		{"cloud metadata address", "https://169.254.169.254/latest/meta-data/"},
		{"no host", "https:///actors/x"},
		{"not a URL", "not a url at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateExternalURL(tc.url); err == nil {
				t.Fatalf("expected %q to be rejected, got nil error", tc.url)
			}
		})
	}
}

func TestValidateExternalURLAllowsPublicHTTPS(t *testing.T) {
	cases := []string{
		"https://example.social/users/alice",
		"https://mastodon.social/users/alice#main-key",
	}
	for _, u := range cases {
		if err := ValidateExternalURL(u); err != nil {
			t.Errorf("expected %q to be allowed, got error: %v", u, err)
		}
	}
}
