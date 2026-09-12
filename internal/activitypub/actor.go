package activitypub

// Actor is the ActivityPub actor object for one series. Field set kept
// intentionally minimal: what Mastodon needs to resolve, display, and
// verify signed requests from this actor.
type Actor struct {
	Context           []string     `json:"@context"`
	ID                string       `json:"id"`
	Type              string       `json:"type"`
	PreferredUsername string       `json:"preferredUsername"`
	Name              string       `json:"name"`
	Summary           string       `json:"summary,omitempty"`
	URL               string       `json:"url,omitempty"`
	Inbox             string       `json:"inbox"`
	Outbox            string       `json:"outbox"`
	Followers         string       `json:"followers"`
	PublicKey         PublicKey    `json:"publicKey"`
	Attachment        []Attachment `json:"attachment,omitempty"`
}

type PublicKey struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	PublicKeyPem string `json:"publicKeyPem"`
}

// Attachment is one profile field, rendered by Mastodon as a labeled value
// on the actor's profile page — used to surface a series' Radicle DID.
type Attachment struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ActorURI returns the canonical actor URI for a series on host.
func ActorURI(host, series string) string {
	return "https://" + host + "/actors/" + series
}

// BuildActor constructs the actor object for a series. repoURL links to
// the underlying repo (its Forgejo or Radicle explorer page); publicKeyPEM
// is that series' stored public key.
func BuildActor(host, series, repoURL, publicKeyPEM string) Actor {
	base := ActorURI(host, series)
	return Actor{
		Context:           []string{"https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"},
		ID:                base,
		Type:              "Service",
		PreferredUsername: series,
		Name:              series,
		Summary:           "Mirrored by graft — commits, issues, and patches for " + series + ".",
		URL:               repoURL,
		Inbox:             base + "/inbox",
		Outbox:            base + "/outbox",
		Followers:         base + "/followers",
		PublicKey: PublicKey{
			ID:           base + "#main-key",
			Owner:        base,
			PublicKeyPem: publicKeyPEM,
		},
	}
}
