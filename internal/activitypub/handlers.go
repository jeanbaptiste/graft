package activitypub

import (
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"graft/internal/state"
)

var actorTmpl = template.Must(template.New("actor").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Handle}} — graft</title>
<style>
  :root { color-scheme: light dark; --brand: #4f7cff; --text-muted: #6b7280; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 40rem; margin: 3rem auto; padding: 0 1.25rem; line-height: 1.5; }
  h1 { font-size: 1.4rem; margin-bottom: .2rem; }
  .sub { color: var(--text-muted); font-size: .9rem; margin-bottom: 1.5rem; }
  .card { border: 1px solid color-mix(in srgb, currentColor 15%, transparent); border-radius: 10px; padding: 1rem 1.25rem; margin-bottom: 1rem; }
  .card a { color: var(--brand); }
  .field { display: flex; gap: .5rem; margin: .3rem 0; font-size: .92rem; }
  .field dt { color: var(--text-muted); min-width: 6rem; }
  ul.activity { list-style: none; padding: 0; margin: 0; }
  ul.activity li { padding: .5rem 0; border-top: 1px solid color-mix(in srgb, currentColor 10%, transparent); font-size: .88rem; }
  ul.activity li:first-child { border-top: none; }
  .kind { display: inline-block; font-size: .72rem; text-transform: uppercase; color: var(--text-muted); margin-right: .4rem; }
  footer { color: var(--text-muted); font-size: .8rem; margin-top: 2rem; }
</style>
</head>
<body>
  <h1>@{{.Handle}}</h1>
  <div class="sub">graft series &middot; {{.Followers}} follower{{if ne .Followers 1}}s{{end}}</div>

  <div class="card">
    <dl>
      {{if .RepoURL}}<div class="field"><dt>Repository</dt><dd><a href="{{.RepoURL}}" target="_blank" rel="noopener">{{.RepoURL}}</a></dd></div>{{end}}
      {{if .RadicleDID}}<div class="field"><dt>Radicle DID</dt><dd>{{.RadicleDID}}</dd></div>{{end}}
    </dl>
  </div>

  <div class="card">
    <strong>Recent activity</strong>
    <ul class="activity">
      {{range .Activity}}
      <li><span class="kind">{{.Kind}}</span>{{if .URL}}<a href="{{.URL}}" target="_blank" rel="noopener">{{.Summary}}</a>{{else}}{{.Summary}}{{end}}</li>
      {{else}}
      <li>nothing mirrored yet</li>
      {{end}}
    </ul>
  </div>

  <footer>
    Fediverse actor for this series &mdash; this same URL serves ActivityPub
    JSON-LD to other fediverse servers.
  </footer>
</body>
</html>
`))

const (
	activityContentType = "application/activity+json"
	outboxLimit         = 50
	maxInboxBody        = 1 << 20 // 1 MiB, generous for a Follow/Undo/reply
)

// Handler serves the ActivityPub surface for every series: WebFinger,
// actor, outbox, individual notes, followers, and an inbox that accepts
// Follow/Undo{Follow} and bridges replies back as Forgejo/Radicle
// comments.
type Handler struct {
	store *state.Store
	host  string
	log   *slog.Logger
	// repoURL, known, postComment, and radicleDID are supplied by the
	// caller (cmd/sync/main.go), which already knows every series'
	// topology and holds the actual sync.RepoSyncer instances — avoids
	// this package needing to know about config.Config or sync.RepoSyncer
	// at all.
	repoURL     func(series string) string
	known       func(series string) bool
	postComment func(repoPair, kind string, forgejoID int64, radicleID, body string) error
	radicleDID  func(series string) string
	client      *http.Client
}

func NewHandler(
	store *state.Store,
	host string,
	log *slog.Logger,
	repoURL func(series string) string,
	known func(series string) bool,
	postComment func(repoPair, kind string, forgejoID int64, radicleID, body string) error,
	radicleDID func(series string) string,
) *Handler {
	return &Handler{
		store:       store,
		host:        host,
		log:         log,
		repoURL:     repoURL,
		known:       known,
		postComment: postComment,
		radicleDID:  radicleDID,
		client:      NewSafeClient(15 * time.Second),
	}
}

// Register mounts every ActivityPub route on mux. Call before mounting any
// catch-all "/" handler — Go's ServeMux resolves the more specific
// patterns registered here first regardless of registration order, but
// keeping this explicit avoids surprises.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/webfinger", h.webfinger)
	mux.HandleFunc("/actors/", h.actorRouter)
}

func (h *Handler) webfinger(w http.ResponseWriter, r *http.Request) {
	series, ok := parseAcct(r.URL.Query().Get("resource"), h.host)
	if !ok || !h.known(series) {
		http.NotFound(w, r)
		return
	}
	resp := map[string]any{
		"subject": "acct:" + series + "@" + h.host,
		"links": []map[string]string{
			{"rel": "self", "type": activityContentType, "href": ActorURI(h.host, series)},
		},
	}
	writeJSON(w, "application/jrd+json", resp)
}

// parseAcct extracts the series name from a WebFinger "acct:series@host"
// resource query, rejecting anything not addressed to our own host.
func parseAcct(resource, host string) (series string, ok bool) {
	resource = strings.TrimPrefix(resource, "acct:")
	at := strings.LastIndex(resource, "@")
	if at < 0 {
		return "", false
	}
	series, resourceHost := resource[:at], resource[at+1:]
	if series == "" || resourceHost != host {
		return "", false
	}
	return series, true
}

func (h *Handler) actorRouter(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/actors/"), "/")
	series := parts[0]
	if series == "" || !h.known(series) {
		http.NotFound(w, r)
		return
	}
	switch {
	case len(parts) == 1:
		h.actor(w, r, series)
	case len(parts) == 2 && parts[1] == "outbox":
		h.outbox(w, series)
	case len(parts) == 2 && parts[1] == "followers":
		h.followers(w, series)
	case len(parts) == 2 && parts[1] == "inbox":
		h.inbox(w, r, series)
	case len(parts) == 3 && parts[1] == "notes":
		h.note(w, r, series, parts[2])
	default:
		http.NotFound(w, r)
	}
}

// actorKeys returns a series' stored keypair, generating and persisting
// one the first time it's asked for.
func (h *Handler) actorKeys(series string) (privPEM, pubPEM string, err error) {
	k, err := h.store.ActorKeys(series)
	if err != nil {
		return "", "", err
	}
	if k != nil {
		return k.PrivateKeyPEM, k.PublicKeyPEM, nil
	}
	priv, pub, err := GenerateKeyPair()
	if err != nil {
		return "", "", err
	}
	if err := h.store.SaveActorKeys(series, state.ActorKeys{PrivateKeyPEM: priv, PublicKeyPEM: pub}); err != nil {
		return "", "", err
	}
	// Re-read: SaveActorKeys is INSERT OR IGNORE, so a concurrent request
	// generating the same series' first keypair may have won the race —
	// this returns whichever copy actually landed in the database.
	k, err = h.store.ActorKeys(series)
	if err != nil {
		return "", "", err
	}
	return k.PrivateKeyPEM, k.PublicKeyPEM, nil
}

func (h *Handler) actor(w http.ResponseWriter, r *http.Request, series string) {
	_, pub, err := h.actorKeys(series)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if prefersHTML(r) {
		h.actorHTML(w, series)
		return
	}
	writeJSON(w, activityContentType, BuildActor(h.host, series, h.repoURL(series), pub, h.radicleDID(series)))
}

// prefersHTML reports whether Accept asks for a browser-readable page
// rather than the ActivityPub JSON-LD representation — the same content
// negotiation Mastodon and other fediverse servers use so a person
// clicking an @handle in a browser lands on a real profile page, while
// another AP server (which sends Accept: application/activity+json or
// application/ld+json) still gets the machine-readable Actor object. A
// request with no Accept header at all (curl, most non-browser clients)
// is treated as wanting JSON, matching this handler's behavior before
// content negotiation existed.
func prefersHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	if strings.Contains(accept, "activity+json") || strings.Contains(accept, "ld+json") {
		return false
	}
	return strings.Contains(accept, "text/html")
}

// actorHTML renders a small human-readable profile page for series: its
// handle, a link to the federated repo, its Radicle DID if any, and its
// most recent mirrored activity — everything already available from the
// same store the JSON Actor/Outbox use.
func (h *Handler) actorHTML(w http.ResponseWriter, series string) {
	entries, err := h.store.ActivityForSeries(series, 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	followers, err := h.store.FollowerCount(series)
	if err != nil {
		followers = 0
	}
	data := actorPageData{
		Handle:     series + "@" + h.host,
		Series:     series,
		RepoURL:    h.repoURL(series),
		RadicleDID: h.radicleDID(series),
		Followers:  followers,
		Activity:   entries,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := actorTmpl.Execute(w, data); err != nil {
		h.log.Error("render actor html", "series", series, "err", err)
	}
}

type actorPageData struct {
	Handle     string
	Series     string
	RepoURL    string
	RadicleDID string
	Followers  int
	Activity   []state.ActivityEntry
}

func (h *Handler) outbox(w http.ResponseWriter, series string) {
	entries, err := h.store.ActivityForSeries(series, outboxLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, activityContentType, BuildOutbox(h.host, series, entries))
}

func (h *Handler) note(w http.ResponseWriter, r *http.Request, series, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	e, err := h.store.ActivityByID(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if e == nil || e.Series != series {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, activityContentType, BuildNote(h.host, series, *e))
}

func (h *Handler) followers(w http.ResponseWriter, series string) {
	n, err := h.store.FollowerCount(series)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, activityContentType, OrderedCollection{
		Context:      "https://www.w3.org/ns/activitystreams",
		ID:           ActorURI(h.host, series) + "/followers",
		Type:         "OrderedCollection",
		TotalItems:   n,
		OrderedItems: []Create{},
	})
}

// actorPrivateKey returns a series' actor's private key, generating a
// keypair first if one doesn't exist yet.
func (h *Handler) actorPrivateKey(series string) (*rsa.PrivateKey, error) {
	privPEM, _, err := h.actorKeys(series)
	if err != nil {
		return nil, err
	}
	return ParsePrivateKey(privPEM)
}

// inboxActivity is the subset of an inbound activity's fields this handler
// reads. Object is kept raw so, for a Follow, it can be embedded verbatim
// as the Accept's object — and so an Undo's nested Follow can be decoded
// separately.
type inboxActivity struct {
	Type   string          `json:"type"`
	Actor  string          `json:"actor"`
	ID     string          `json:"id"`
	Object json.RawMessage `json:"object"`
}

func (h *Handler) inbox(w http.ResponseWriter, r *http.Request, series string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboxBody))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var act inboxActivity
	if err := json.Unmarshal(body, &act); err != nil || act.Actor == "" {
		http.Error(w, "invalid activity", http.StatusBadRequest)
		return
	}

	remoteActor, err := FetchActor(h.client, act.Actor)
	if err != nil {
		h.log.Error("ap inbox: resolve actor", "series", series, "actor", act.Actor, "err", err)
		http.Error(w, "could not resolve actor", http.StatusBadGateway)
		return
	}
	pub, err := ParsePublicKey(remoteActor.PublicKey.PublicKeyPem)
	if err != nil {
		http.Error(w, "invalid actor public key", http.StatusBadGateway)
		return
	}
	if err := VerifyRequest(r, body, pub); err != nil {
		h.log.Error("ap inbox: signature verification failed", "series", series, "actor", act.Actor, "err", err)
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	switch act.Type {
	case "Follow":
		if err := h.store.SaveFollower(series, act.Actor, remoteActor.Inbox); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.sendAccept(series, remoteActor.Inbox, body); err != nil {
			// The follow is already recorded; a failed Accept delivery
			// shouldn't undo that — the remote side will likely retry.
			h.log.Error("ap inbox: send accept", "series", series, "actor", act.Actor, "err", err)
		}
	case "Undo":
		var inner inboxActivity
		if err := json.Unmarshal(act.Object, &inner); err == nil && inner.Type == "Follow" {
			if err := h.store.RemoveFollower(series, act.Actor); err != nil {
				h.log.Error("ap inbox: remove follower", "series", series, "actor", act.Actor, "err", err)
			}
		}
	case "Create":
		var note inboxNote
		if err := json.Unmarshal(act.Object, &note); err == nil && note.Type == "Note" && note.InReplyTo != "" {
			h.handleReply(series, remoteActor, note)
		}
	default:
		// Anything else (Like, Announce, ...) is accepted and ignored.
	}

	w.WriteHeader(http.StatusAccepted)
}

// inboxNote is the subset of an inbound reply's Note object this handler
// reads.
type inboxNote struct {
	Type      string `json:"type"`
	InReplyTo string `json:"inReplyTo"`
	Content   string `json:"content"`
}

// handleReply bridges a fediverse reply to one of our Notes back onto the
// Forgejo issue/PR and Radicle issue/patch it's actually about. Create-only
// and best-effort, same trust model as the rest of graft's mirroring: never
// blocks or fails the inbox response, just logs and moves on.
func (h *Handler) handleReply(series string, remoteActor *Actor, note inboxNote) {
	noteSeries, entryID, ok := ParseNoteURI(h.host, note.InReplyTo)
	if !ok || noteSeries != series {
		return
	}
	e, err := h.store.ActivityByID(entryID)
	if err != nil {
		h.log.Error("ap reply: load activity", "series", series, "id", entryID, "err", err)
		return
	}
	if e == nil || (e.Kind != "issue" && e.Kind != "patch") {
		return // nothing to comment on — a reply to a git commit note, or the entry vanished
	}

	who := remoteActor.PreferredUsername
	if who == "" {
		who = remoteActor.Name
	}
	body := state.MarkMirrored(fmt.Sprintf("**via Fediverse, @%s:**\n\n%s", who, stripHTML(note.Content)))

	if err := h.postComment(e.RepoPair, e.Kind, e.ForgejoID, e.RadicleID, body); err != nil {
		h.log.Error("ap reply: post comment", "series", series, "repo_pair", e.RepoPair, "err", err)
		return
	}
	if _, err := h.store.LogActivity(state.Activity{
		RepoPair: e.RepoPair, Series: e.Series, SeriesURL: e.SeriesURL,
		Kind: "comment", Direction: e.Direction, Origin: "activitypub",
		Summary:   truncateSummary(note.Content),
		URL:       e.URL,
		ForgejoID: e.ForgejoID, RadicleID: e.RadicleID,
	}); err != nil {
		h.log.Error("ap reply: log activity", "series", series, "repo_pair", e.RepoPair, "err", err)
	}
}

// truncateSummary bounds how much of a fediverse reply's raw HTML content
// lands in the dashboard's summary column.
func truncateSummary(s string) string {
	s = stripHTML(s)
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// sendAccept replies to a Follow with a signed Accept, embedding the
// original Follow activity verbatim as required by the spec.
func (h *Handler) sendAccept(series, targetInbox string, followBody []byte) error {
	priv, err := h.actorPrivateKey(series)
	if err != nil {
		return err
	}
	actor := ActorURI(h.host, series)
	accept := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       actor + "/accepts/" + strconv.FormatInt(time.Now().UnixNano(), 10),
		"type":     "Accept",
		"actor":    actor,
		"object":   json.RawMessage(followBody),
	}
	body, err := json.Marshal(accept)
	if err != nil {
		return err
	}
	return PostSigned(h.client, targetInbox, actor+"#main-key", priv, body)
}

// DeliverNewActivity signs and POSTs a Create{Note} to every follower's
// inbox for each series' activity_log rows logged since the last
// delivery. Call it once per sync pass, after runAll has logged whatever
// happened this pass. Best-effort: a delivery failure to one follower is
// logged and skipped, never blocks the sync loop or other followers.
func (h *Handler) DeliverNewActivity() {
	seriesList, err := h.store.SeriesWithFollowers()
	if err != nil {
		h.log.Error("ap delivery: list series with followers", "err", err)
		return
	}

	for _, series := range seriesList {
		cursor, err := h.store.DeliveryCursor(series)
		if err != nil {
			h.log.Error("ap delivery: read cursor", "series", series, "err", err)
			continue
		}
		entries, err := h.store.NewActivityForSeries(series, cursor)
		if err != nil {
			h.log.Error("ap delivery: list new activity", "series", series, "err", err)
			continue
		}
		if len(entries) == 0 {
			continue
		}

		followers, err := h.store.Followers(series)
		if err != nil {
			h.log.Error("ap delivery: list followers", "series", series, "err", err)
			continue
		}
		priv, err := h.actorPrivateKey(series)
		if err != nil {
			h.log.Error("ap delivery: load actor key", "series", series, "err", err)
			continue
		}
		keyID := ActorURI(h.host, series) + "#main-key"

		maxID := cursor
		for _, e := range entries {
			note := BuildNote(h.host, series, e)
			create := Create{
				Context:   "https://www.w3.org/ns/activitystreams",
				ID:        note.ID + "/activity",
				Type:      "Create",
				Actor:     ActorURI(h.host, series),
				Published: note.Published,
				To:        []string{publicAudience},
				Object:    note,
			}
			body, err := json.Marshal(create)
			if err != nil {
				h.log.Error("ap delivery: marshal activity", "series", series, "id", e.ID, "err", err)
				continue
			}
			for _, f := range followers {
				// Defense in depth: FetchActor already validates an
				// inbox URL before a follower is ever stored, but check
				// again here too, in case older data predates that
				// validation or was written some other way.
				if err := ValidateExternalURL(f.InboxURI); err != nil {
					h.log.Error("ap delivery: refusing unsafe follower inbox", "series", series, "follower", f.ActorURI, "err", err)
					continue
				}
				if err := PostSigned(h.client, f.InboxURI, keyID, priv, body); err != nil {
					h.log.Error("ap delivery: post to follower", "series", series, "follower", f.ActorURI, "err", err)
				}
			}
			if e.ID > maxID {
				maxID = e.ID
			}
		}
		if err := h.store.SetDeliveryCursor(series, maxID); err != nil {
			h.log.Error("ap delivery: set cursor", "series", series, "err", err)
		}
	}
}

func writeJSON(w http.ResponseWriter, contentType string, v any) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
