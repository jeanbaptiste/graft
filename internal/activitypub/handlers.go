package activitypub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"graft/internal/state"
)

const (
	activityContentType = "application/activity+json"
	outboxLimit         = 50
)

// Handler serves the ActivityPub surface for every series: WebFinger,
// actor, outbox, individual notes, and (for now, always empty — see M2 in
// the v2 plan) a followers collection.
type Handler struct {
	store *state.Store
	host  string
	// repoURL and known are supplied by the caller (cmd/sync/main.go),
	// which already knows every series' topology — avoids this package
	// needing to know about config.Config or sync.RepoSyncer at all.
	repoURL func(series string) string
	known   func(series string) bool
}

func NewHandler(store *state.Store, host string, repoURL func(series string) string, known func(series string) bool) *Handler {
	return &Handler{store: store, host: host, repoURL: repoURL, known: known}
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
		h.actor(w, series)
	case len(parts) == 2 && parts[1] == "outbox":
		h.outbox(w, series)
	case len(parts) == 2 && parts[1] == "followers":
		h.followers(w, series)
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

func (h *Handler) actor(w http.ResponseWriter, series string) {
	_, pub, err := h.actorKeys(series)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, activityContentType, BuildActor(h.host, series, h.repoURL(series), pub))
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
	writeJSON(w, activityContentType, OrderedCollection{
		Context:      "https://www.w3.org/ns/activitystreams",
		ID:           ActorURI(h.host, series) + "/followers",
		Type:         "OrderedCollection",
		TotalItems:   0,
		OrderedItems: []Create{},
	})
}

func writeJSON(w http.ResponseWriter, contentType string, v any) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
