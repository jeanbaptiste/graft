package sync

import "testing"

func TestCommitSHA(t *testing.T) {
	cases := []struct{ summary, url, want string }{
		{"4edbad6 Ajouter docs/img/README.md", "https://f1.example/o/r/commit/4edbad669fc52f8a", "4edbad6"},
		{"", "https://radicle.example/nodes/r1/rad:z/commits/42ab05973e157792962b", "42ab059"},
		{"Merge branch main", "https://f2.example/o/r/commit/ee7e6a82deb4b25c", "ee7e6a8"},
		{"not a commit", "https://f1.example/o/r/issues/3", ""},
	}
	for _, c := range cases {
		if got := commitSHA(c.summary, c.url); got != c.want {
			t.Errorf("commitSHA(%q, %q) = %q, want %q", c.summary, c.url, got, c.want)
		}
	}
}
