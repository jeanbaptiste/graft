package admin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

const (
	shareTTL         = 15 * time.Minute
	maxShareAttempts = 5
	maxSecretBytes   = 8000
)

// newShareID returns 16 random bytes, base64url-encoded (no padding) —
// the only copy of it that ever exists in reversible form is the one
// handed to whoever creates the share; the database only ever sees its
// hash (see hashShareID).
func newShareID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashShareID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

// newPasscode returns a uniformly random 6-digit code, zero-padded.
func newPasscode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// shareKey derives the AES-256 key for one share from its id and
// passcode together — never from either alone. A database dump gives an
// attacker id_hash and ciphertext but never the plaintext id, so it's
// insufficient by itself; an attacker who only has the link (the
// plaintext id) still needs the passcode, which is never stored at all —
// GCM's authentication tag is what tells a wrong-passcode attempt apart
// from a right one, not a stored comparison value.
func shareKey(id, passcode string) []byte {
	mac := hmac.New(sha256.New, []byte(id))
	mac.Write([]byte("graft-share-v1|" + passcode))
	return mac.Sum(nil)
}

func encryptShare(id, passcode, plaintext string) (nonceB64, ciphertextB64 string, err error) {
	block, err := aes.NewCipher(shareKey(id, passcode))
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(ct), nil
}

func decryptShare(id, passcode, nonceB64, ciphertextB64 string) (string, error) {
	block, err := aes.NewCipher(shareKey(id, passcode))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce, err := base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return "", err
	}
	ct, err := base64.StdEncoding.DecodeString(ciphertextB64)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil) // fails (wrong passcode) is the expected common case, not a real error
	if err != nil {
		return "", errWrongPasscode
	}
	return string(pt), nil
}

var errWrongPasscode = fmt.Errorf("wrong passcode")

func (h *Handler) shareBaseURL(r *http.Request) string {
	if h.cfg.PublicHost != "" {
		return "https://" + h.cfg.PublicHost
	}
	return "https://" + r.Host
}

var shareNewTmpl = template.Must(template.New("share-new").Parse(`
<h1>Share a secret, once</h1>
<p class="lead">Paste a token or anything else sensitive below. You'll get back a one-time link and a separate 6-digit passcode — hand the link and the passcode to the recipient over two <em>different</em> channels (a link over chat, the passcode read aloud on a call, say), so intercepting one alone is useless. The secret is encrypted, shown to the recipient exactly once, and then destroyed — including if they enter the wrong passcode five times.</p>
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
<div class="card">
  <form method="POST" action="/share/new">
    <label for="secret">Secret</label>
    <textarea id="secret" name="secret" required maxlength="8000" placeholder="the token, pasted as-is"></textarea>
    <div class="hint">Never stored in reversible form without a passcode only the recipient will have.</div>
    <button type="submit">Create one-time link</button>
  </form>
</div>
`))

var shareCreatedTmpl = template.Must(template.New("share-created").Parse(`
<h1>Share created</h1>
<div class="notice good">This is the only time both the link and passcode are shown together. Send them to the recipient now, over two separate channels.</div>
<div class="card">
  <label>One-time link</label>
  <div class="secret-box"><a href="{{.URL}}">{{.URL}}</a></div>
  <label>Passcode</label>
  <div class="passcode">{{.Passcode}}</div>
  <label>QR code (opens the link — the recipient still enters the passcode by hand)</label>
  <div style="text-align:center;margin-top:.5rem;"><img src="/share/{{.ID}}/qr.png" width="200" height="200" alt="QR code for the one-time link"></div>
  <div class="hint">Expires in 15 minutes if never claimed. Claimed exactly once — a second visit shows nothing.</div>
</div>
`))

var shareClaimTmpl = template.Must(template.New("share-claim").Parse(`
<h1>Claim a shared secret</h1>
<p class="lead">Enter the 6-digit passcode you were given separately from this link. It can only be tried a handful of times before the secret is destroyed.</p>
{{if .Error}}<div class="notice bad">{{.Error}}</div>{{end}}
<div class="card">
  <form method="POST" action="/share/{{.ID}}">
    <label for="passcode">Passcode</label>
    <input type="text" id="passcode" name="passcode" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required autofocus>
    <button type="submit">Reveal</button>
  </form>
</div>
`))

var shareRevealedTmpl = template.Must(template.New("share-revealed").Parse(`
<h1>Secret</h1>
<div class="notice good">Shown once. It's already deleted — copy it now, this page won't show it again on refresh.</div>
<div class="card">
  <div class="secret-box">{{.Secret}}</div>
</div>
`))

func (h *Handler) shareNew(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		render(w, "share a secret", "ONE-TIME SHARE", renderFragment(shareNewTmpl, map[string]string{}))
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			render(w, "share a secret", "ONE-TIME SHARE", renderFragment(shareNewTmpl, map[string]string{"Error": "couldn't read the form"}))
			return
		}
		secret := strings.TrimSpace(r.FormValue("secret"))
		if secret == "" {
			render(w, "share a secret", "ONE-TIME SHARE", renderFragment(shareNewTmpl, map[string]string{"Error": "the secret can't be empty"}))
			return
		}
		if len(secret) > maxSecretBytes {
			render(w, "share a secret", "ONE-TIME SHARE", renderFragment(shareNewTmpl, map[string]string{"Error": "that's too long for a one-time share"}))
			return
		}
		id, err := newShareID()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		passcode, err := newPasscode()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		nonce, ct, err := encryptShare(id, passcode, secret)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if err := h.cfg.Store.CreateTokenShare(hashShareID(id), nonce, ct, time.Now().Add(shareTTL)); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		render(w, "share created", "ONE-TIME SHARE", renderFragment(shareCreatedTmpl, map[string]string{
			"URL":      h.shareBaseURL(r) + "/share/" + id,
			"Passcode": passcode,
			"ID":       id,
		}))
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) shareClaim(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/share/")
	parts := strings.Split(path, "/")
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 && parts[1] == "qr.png" {
		h.serveShareQR(w, r, id)
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		render(w, "claim a secret", "ONE-TIME SHARE", renderFragment(shareClaimTmpl, map[string]string{"ID": id}))
	case http.MethodPost:
		h.claimShare(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) serveShareQR(w http.ResponseWriter, r *http.Request, id string) {
	// The QR encodes only the claim link, never the passcode — scanning
	// it is a convenience for reaching the page in person or on a call,
	// not a second channel in itself; the passcode is still meant to be
	// told separately.
	png, err := qrcode.Encode(h.shareBaseURL(r)+"/share/"+id, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "could not generate QR code", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (h *Handler) claimShare(w http.ResponseWriter, r *http.Request, id string) {
	ip := clientIP(r)
	if !h.shareLimiter.allowed(ip) {
		render(w, "claim a secret", "ONE-TIME SHARE", renderFragment(shareClaimTmpl, map[string]string{
			"ID": id, "Error": "too many attempts from your address — try again later",
		}))
		return
	}
	if err := r.ParseForm(); err != nil {
		render(w, "claim a secret", "ONE-TIME SHARE", renderFragment(shareClaimTmpl, map[string]string{"ID": id, "Error": "couldn't read the form"}))
		return
	}
	passcode := strings.TrimSpace(r.FormValue("passcode"))

	idHash := hashShareID(id)
	share, err := h.cfg.Store.GetTokenShare(idHash)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if share == nil {
		render(w, "claim a secret", "ONE-TIME SHARE", renderFragment(shareClaimTmpl, map[string]string{
			"ID": id, "Error": "this link is invalid, already claimed, or expired",
		}))
		return
	}

	secret, err := decryptShare(id, passcode, share.Nonce, share.Ciphertext)
	if err != nil {
		h.shareLimiter.recordFailure(ip)
		attempts, aerr := h.cfg.Store.IncrementTokenShareAttempts(idHash)
		if aerr == nil && attempts >= maxShareAttempts {
			_ = h.cfg.Store.DeleteTokenShare(idHash)
			render(w, "claim a secret", "ONE-TIME SHARE", renderFragment(shareClaimTmpl, map[string]string{
				"ID": id, "Error": "too many wrong passcodes — this secret has been destroyed",
			}))
			return
		}
		render(w, "claim a secret", "ONE-TIME SHARE", renderFragment(shareClaimTmpl, map[string]string{
			"ID": id, "Error": "wrong passcode",
		}))
		return
	}

	// One-time by design: destroy it the moment it's successfully read,
	// before rendering the response that reveals it.
	_ = h.cfg.Store.DeleteTokenShare(idHash)
	render(w, "secret", "ONE-TIME SHARE", renderFragment(shareRevealedTmpl, map[string]string{"Secret": secret}))
}
