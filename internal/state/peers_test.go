package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPeersFileRoundTripRestoresLostPeers(t *testing.T) {
	dir := t.TempDir()
	peers := filepath.Join(dir, "peers.yaml")

	st, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	approved := DynamicRepo{
		Name: "federation-x-f3", Series: "federation-x",
		ForgejoBaseURL: "https://f3.example", ForgejoOwner: "o", ForgejoRepo: "r", ForgejoTokenFile: "/etc/graft/f3.token",
		RadicleRID: "rad:z3", RadicleHTTPBaseURL: "https://seed.example", RadicleRadHome: "/home/graft/.radicle",
		SyncGit: true, SyncIssues: true, SyncPatches: false,
	}
	id, err := st.CreateDynamicRepo(approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApproveDynamicRepo(id); err != nil {
		t.Fatal(err)
	}
	pending := approved
	pending.Name, pending.ForgejoRepo = "still-pending", "p"
	if _, err := st.CreateDynamicRepo(pending); err != nil {
		t.Fatal(err)
	}

	changed, err := st.ExportDynamicRepos(peers)
	if err != nil || !changed {
		t.Fatalf("first export: changed=%v err=%v", changed, err)
	}
	if info, err := os.Stat(peers); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("peers file mode: %v %v", info, err)
	}
	if changed, err := st.ExportDynamicRepos(peers); err != nil || changed {
		t.Fatalf("unchanged export rewrote the file: changed=%v err=%v", changed, err)
	}
	st.Close()

	// A brand-new, empty state.db — what a lost or wiped database looks like.
	fresh, err := Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	restored, err := fresh.ImportDynamicRepos(peers)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != "federation-x-f3" {
		t.Fatalf("restored %v, want only the approved peer", restored)
	}
	rows, err := fresh.AllDynamicRepos()
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows after import: %v %v", rows, err)
	}
	got := rows[0]
	if got.ForgejoTokenFile != approved.ForgejoTokenFile || got.RadicleRID != approved.RadicleRID || got.SyncPatches || !got.Approved || got.Materialized {
		t.Fatalf("imported row differs: %+v", got)
	}
	if again, err := fresh.ImportDynamicRepos(peers); err != nil || len(again) != 0 {
		t.Fatalf("second import should be a no-op: %v %v", again, err)
	}
	if missing, err := fresh.ImportDynamicRepos(filepath.Join(dir, "nope.yaml")); err != nil || missing != nil {
		t.Fatalf("missing peers file: %v %v", missing, err)
	}
}
