package activitypub

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

// TestVerifyRejectsStaleDate confirms a byte-for-byte replay of an old,
// genuinely-validly-signed request is rejected: the signature itself
// verifies fine (Date is part of what was signed, and hasn't changed —
// this is a faithful replay, not tampering), but the freshness check must
// still catch that "now" is far past the signed Date. SignRequest always
// stamps the current time, so this builds the signature by hand with a
// fixed old Date to simulate a captured-and-replayed request correctly.
func TestVerifyRejectsStaleDate(t *testing.T) {
	_, pub, priv := testKeyPair(t)
	body := []byte(`{"type":"Follow","actor":"https://example.social/users/alice"}`)
	keyID := "https://example.social/users/alice#main-key"

	req, err := http.NewRequest(http.MethodPost, "https://graft.example.org/actors/testrepo/inbox", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	oldDate := time.Now().Add(-1 * time.Hour).UTC().Format(http.TimeFormat)
	digest := sha256.Sum256(body)
	req.Header.Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(digest[:]))
	req.Header.Set("Date", oldDate)

	signingString := buildSigningString(signedHeaders, req.Method, req.URL.RequestURI(), req.URL.Host, req.Header)
	hashed := sha256.Sum256([]byte(signingString))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Signature", fmt.Sprintf(
		`keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		keyID, strings.Join(signedHeaders, " "), base64.StdEncoding.EncodeToString(sig)))

	if err := VerifyRequest(req, body, pub); err == nil {
		t.Fatal("expected verification to fail on a stale/replayed Date header, got nil error")
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
