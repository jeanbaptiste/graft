package main

import (
	"reflect"
	"testing"

	"graft/internal/config"
	"graft/internal/state"
)

func TestDefaultReposUnchanged(t *testing.T) {
	got, err := parseRepoSpecs(defaultRepos)
	if err != nil {
		t.Fatal(err)
	}
	want := []repoSpec{
		{"forgeadmin", "constitution", "constitution", ""},
		{"forgeadmin", "graft", "graft-source", ""},
		{"forgeadmin", "federation-x", "federation-x", ""},
		{"forgeadmin", "graft-test-v2", "graft-test-v2", ""},
		{"forgeadmin", "graft-presentation", "graft-presentation", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default repos changed:\n got  %+v\n want %+v", got, want)
	}
	if _, err := parseRepoSpecs("nope"); err == nil {
		t.Fatal("malformed entry accepted")
	}
}

func TestReposFromGraftKeepsOnlyBaseInstance(t *testing.T) {
	f1 := "https://f1.cyberwild.org"
	pairs := []config.RepoPair{
		{Name: "graft-source-f1", Series: "graft-source", Forgejo: config.ForgejoTarget{BaseURL: f1, Owner: "forgeadmin", Repo: "graft", TokenFile: "/etc/graft/f1.token"}},
		{Name: "graft-source-f2", Series: "graft-source", Forgejo: config.ForgejoTarget{BaseURL: "https://f2.cyberwild.org", Owner: "forgeadmin", Repo: "graft"}},
		{Name: "graft-presentation", Series: "graft-presentation",
			Forgejo:       config.ForgejoTarget{BaseURL: f1 + "/", Owner: "forgeadmin", Repo: "graft-presentation", TokenFile: "/etc/graft/f1.token"},
			ForgejoMirror: &config.ForgejoTarget{BaseURL: "https://artefacts.bimr.net", Owner: "alice", Repo: "graft-presentation"}},
		{Name: "dup", Series: "graft-source", Forgejo: config.ForgejoTarget{BaseURL: f1, Owner: "forgeadmin", Repo: "graft"}},
	}
	peers := []state.DynamicRepo{
		{Series: "constitution", ForgejoBaseURL: "https://git.tricoteuses.fr", ForgejoOwner: "constitution", ForgejoRepo: "c"},
		{Series: "federation-x", ForgejoBaseURL: f1, ForgejoOwner: "forgeadmin", ForgejoRepo: "federation-x", ForgejoTokenFile: "/etc/graft/fx.token"},
	}
	got := reposFromGraft(f1, pairs, peers)
	want := []repoSpec{
		{"forgeadmin", "graft", "graft-source", "/etc/graft/f1.token"},
		{"forgeadmin", "graft-presentation", "graft-presentation", "/etc/graft/f1.token"},
		{"forgeadmin", "federation-x", "federation-x", "/etc/graft/fx.token"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}
