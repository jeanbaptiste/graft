package config

import (
	"testing"
	"time"
)

func TestIntervalForSlowsThirdPartyHosts(t *testing.T) {
	c := &Config{
		SyncInterval: 15 * time.Second,
		HostSyncIntervals: map[string]time.Duration{
			"git.tricoteuses.fr": 5 * time.Minute,
			"artefacts.bimr.net": 5 * time.Minute,
			"fast.example":       time.Second,
		},
	}
	cases := []struct {
		name string
		pair RepoPair
		want time.Duration
	}{
		{"own instance", RepoPair{Forgejo: ForgejoTarget{BaseURL: "https://f1.cyberwild.org"}}, 15 * time.Second},
		{"third party", RepoPair{Forgejo: ForgejoTarget{BaseURL: "https://git.tricoteuses.fr/"}}, 5 * time.Minute},
		{"third-party mirror", RepoPair{Forgejo: ForgejoTarget{BaseURL: "https://f1.cyberwild.org"}, ForgejoMirror: &ForgejoTarget{BaseURL: "https://artefacts.bimr.net"}}, 5 * time.Minute},
		{"override shorter than global", RepoPair{Forgejo: ForgejoTarget{BaseURL: "https://fast.example"}}, 15 * time.Second},
	}
	for _, tc := range cases {
		if got := c.IntervalFor(tc.pair); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
