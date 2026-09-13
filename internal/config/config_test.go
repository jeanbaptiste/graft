package config

import "testing"

func validPair() RepoPair {
	return RepoPair{
		Name:    "p",
		Forgejo: ForgejoTarget{BaseURL: "https://f1", Owner: "o", Repo: "r", TokenFile: "/tmp/t"},
		Sync:    SyncScope{Git: true},
	}
}

func TestValidateRequiresRadicleOrForgejoMirror(t *testing.T) {
	p := validPair()
	if err := p.validate(); err == nil {
		t.Fatal("expected error when neither radicle nor forgejo_mirror is set")
	}
}

func TestValidateRejectsBothRadicleAndForgejoMirror(t *testing.T) {
	p := validPair()
	p.Radicle = &RadicleTarget{RID: "rad:z1", HTTPBaseURL: "https://r1", RadHome: "/home/r"}
	p.ForgejoMirror = &ForgejoTarget{BaseURL: "https://f2", Owner: "o", Repo: "r", TokenFile: "/tmp/t2"}
	if err := p.validate(); err == nil {
		t.Fatal("expected error when both radicle and forgejo_mirror are set")
	}
}

func TestValidateAcceptsRadicleOnly(t *testing.T) {
	p := validPair()
	p.Radicle = &RadicleTarget{RID: "rad:z1", HTTPBaseURL: "https://r1", RadHome: "/home/r"}
	if err := p.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateAcceptsForgejoMirrorOnly(t *testing.T) {
	p := validPair()
	p.ForgejoMirror = &ForgejoTarget{BaseURL: "https://f2", Owner: "o", Repo: "r", TokenFile: "/tmp/t2"}
	if err := p.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsIssuesOrPatchesOnForgejoMirror(t *testing.T) {
	p := validPair()
	p.ForgejoMirror = &ForgejoTarget{BaseURL: "https://f2", Owner: "o", Repo: "r", TokenFile: "/tmp/t2"}
	p.Sync.Issues = true
	if err := p.validate(); err == nil {
		t.Fatal("expected error: forgejo_mirror pairs don't support issue sync")
	}
}
