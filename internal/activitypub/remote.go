package activitypub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// FetchActor retrieves a remote actor object. actorURL is attacker
// influenced (it comes straight off an inbound activity's "actor" field),
// so it's validated before any network call, and client is expected to be
// one from NewSafeClient. An unsigned GET is used — acceptable here since
// this is only for resolving a Follow request's actor (to get their
// public key and inbox URL) and most servers permit unauthenticated actor
// fetches; authenticated fetches ("secure mode") are out of scope for now.
func FetchActor(client *http.Client, actorURL string) (*Actor, error) {
	if err := ValidateExternalURL(actorURL); err != nil {
		return nil, fmt.Errorf("actor URL: %w", err)
	}

	req, err := http.NewRequest(http.MethodGet, actorURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", activityContentType)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch actor %s: %w", actorURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch actor %s: HTTP %d", actorURL, resp.StatusCode)
	}

	var a Actor
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&a); err != nil {
		return nil, fmt.Errorf("decode actor %s: %w", actorURL, err)
	}
	if err := ValidateExternalURL(a.Inbox); err != nil {
		return nil, fmt.Errorf("actor inbox URL: %w", err)
	}
	return &a, nil
}
