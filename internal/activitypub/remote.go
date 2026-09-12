package activitypub

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// FetchActor retrieves a remote actor object. An unsigned GET is used —
// acceptable here since this is only for resolving a Follow request's
// actor (to get their public key and inbox URL) and most servers permit
// unauthenticated actor fetches; authenticated fetches ("secure mode") are
// out of scope for now.
func FetchActor(client *http.Client, actorURL string) (*Actor, error) {
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
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return nil, fmt.Errorf("decode actor %s: %w", actorURL, err)
	}
	return &a, nil
}
