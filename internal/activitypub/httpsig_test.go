package activitypub

import (
	"bytes"
	"crypto/rsa"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSignVerifyRoundTrip exercises the actual code path a remote server
// runs: sign a POST the way PostSigned does, receive it through an
// httptest server, and verify it the way the inbox handler does. This is
// the one piece of this package genuinely worth distrusting until proven —
// HTTP Signatures are notoriously easy to get subtly wrong (header casing,
// request-target format, digest encoding) in a way that only shows up
// against a real remote server's strict verifier.
func TestSignVerifyRoundTrip(t *testing.T) {
	_, pub, priv := testKeyPair(t)

	body := []byte(`{"type":"Follow","actor":"https://example.social/users/alice","object":"https://graft.example.org/actors/testrepo"}`)

	var verifyErr error
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		verifyErr = VerifyRequest(r, gotBody, pub)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := srv.Client()
	if err := PostSigned(client, srv.URL+"/actors/testrepo/inbox", "https://example.social/users/alice#main-key", priv, body); err != nil {
		t.Fatalf("PostSigned: %v", err)
	}
	if verifyErr != nil {
		t.Fatalf("VerifyRequest: %v", verifyErr)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("body mismatch: got %q want %q", gotBody, body)
	}
}

// TestVerifyRejectsTamperedBody confirms a body modified in transit (or by
// a malicious intermediary) fails verification via the Digest mismatch,
// even though the Signature header itself is untouched.
func TestVerifyRejectsTamperedBody(t *testing.T) {
	_, pub, priv := testKeyPair(t)
	body := []byte(`{"type":"Follow","actor":"https://example.social/users/alice"}`)

	req, err := http.NewRequest(http.MethodPost, "https://graft.example.org/actors/testrepo/inbox", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := SignRequest(req, body, "https://example.social/users/alice#main-key", priv); err != nil {
		t.Fatal(err)
	}

	tampered := []byte(`{"type":"Follow","actor":"https://attacker.example/users/mallory"}`)
	if err := VerifyRequest(req, tampered, pub); err == nil {
		t.Fatal("expected verification to fail on tampered body, got nil error")
	}
}

func testKeyPair(t *testing.T) (privPEM string, pub *rsa.PublicKey, priv *rsa.PrivateKey) {
	t.Helper()
	privateP, publicP, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	privKey, err := ParsePrivateKey(privateP)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	pubKey, err := ParsePublicKey(publicP)
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	return privateP, pubKey, privKey
}
