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
	"strings"
	"time"
)

// signedHeaders is the fixed set of headers this package signs and
// verifies — the subset draft-cavage HTTP Signatures needs to bind a
// request to its actual method, path, host, date, and body, which is what
// every mainstream ActivityPub implementation (Mastodon included) expects.
var signedHeaders = []string{"(request-target)", "host", "date", "digest"}

// SignRequest signs req per draft-cavage HTTP Signatures: (request-target),
// host, date, and a Digest of body are covered. Sets the Digest, Date, and
// Signature headers on req.
func SignRequest(req *http.Request, body []byte, keyID string, priv *rsa.PrivateKey) error {
	digest := sha256.Sum256(body)
	req.Header.Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(digest[:]))
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))

	signingString := buildSigningString(signedHeaders, req.Method, req.URL.RequestURI(), req.URL.Host, req.Header)
	hashed := sha256.Sum256([]byte(signingString))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, hashed[:])
	if err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	req.Header.Set("Signature", fmt.Sprintf(
		`keyId="%s",algorithm="rsa-sha256",headers="%s",signature="%s"`,
		keyID, strings.Join(signedHeaders, " "), base64.StdEncoding.EncodeToString(sig),
	))
	return nil
}

// VerifyRequest verifies an inbound request's Signature header against
// pub, re-deriving the signing string from whichever headers the
// signature itself claims to cover (a remote server may sign a different
// header set than we do when sending).
func VerifyRequest(r *http.Request, body []byte, pub *rsa.PublicKey) error {
	params := parseSignatureHeader(r.Header.Get("Signature"))
	if params["signature"] == "" {
		return fmt.Errorf("missing or unparseable Signature header")
	}

	if digestHeader := r.Header.Get("Digest"); digestHeader != "" {
		sum := sha256.Sum256(body)
		want := "SHA-256=" + base64.StdEncoding.EncodeToString(sum[:])
		if !strings.EqualFold(digestHeader, want) {
			return fmt.Errorf("digest does not match body")
		}
	}

	headers := strings.Fields(params["headers"])
	if len(headers) == 0 {
		headers = []string{"date"}
	}
	host := r.Header.Get("Host")
	if host == "" {
		host = r.Host
	}
	signingString := buildSigningString(headers, r.Method, r.URL.RequestURI(), host, r.Header)

	sigBytes, err := base64.StdEncoding.DecodeString(params["signature"])
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	hashed := sha256.Sum256([]byte(signingString))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, hashed[:], sigBytes); err != nil {
		return fmt.Errorf("signature does not verify: %w", err)
	}
	return nil
}

// KeyID extracts the keyId parameter from an inbound request's Signature
// header — usually the remote actor's URI with a #main-key fragment,
// telling us whose public key to fetch and verify against.
func KeyID(r *http.Request) string {
	return parseSignatureHeader(r.Header.Get("Signature"))["keyId"]
}

func buildSigningString(headers []string, method, requestURI, host string, reqHeaders http.Header) string {
	var lines []string
	for _, h := range headers {
		switch strings.ToLower(h) {
		case "(request-target)":
			lines = append(lines, fmt.Sprintf("(request-target): %s %s", strings.ToLower(method), requestURI))
		case "host":
			lines = append(lines, "host: "+host)
		default:
			lines = append(lines, strings.ToLower(h)+": "+reqHeaders.Get(h))
		}
	}
	return strings.Join(lines, "\n")
}

func parseSignatureHeader(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		out[kv[0]] = strings.Trim(kv[1], `"`)
	}
	return out
}

// PostSigned sends a signed POST of an activity+json body, authenticated
// as keyID using priv.
func PostSigned(client *http.Client, url, keyID string, priv *rsa.PrivateKey, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", activityContentType)
	if err := SignRequest(req, body, keyID, priv); err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post %s: HTTP %d: %s", url, resp.StatusCode, string(respBody))
	}
	return nil
}
