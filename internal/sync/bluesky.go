package sync

import (
	"fmt"

	"graft/internal/atproto"
)

// BlueskyPoster posts short notes to one Bluesky account on behalf of a
// repo pair. Nil-safe: a nil *BlueskyPoster's Post is a silent no-op, so
// every call site can post unconditionally instead of nil-checking —
// matching the "absent config = feature off, never an error" rule for
// this integration (see config.RepoPair.Bluesky).
type BlueskyPoster struct {
	Handle      string
	AppPassword string
	PDSHost     string
}

// Post logs in fresh and publishes text, returning the new post's AT-URI
// (used to recognize a later reply to it). A new session per post is
// wasteful but simple and avoids session-expiry bookkeeping — acceptable
// given how infrequently a single pair mirrors new content.
func (b *BlueskyPoster) Post(text string) (uri string, err error) {
	if b == nil {
		return "", nil
	}
	c := atproto.New(b.PDSHost)
	if err := c.Login(b.Handle, b.AppPassword); err != nil {
		return "", fmt.Errorf("bluesky login: %w", err)
	}
	uri, err = c.Post(text)
	if err != nil {
		return "", fmt.Errorf("bluesky post: %w", err)
	}
	return uri, nil
}
