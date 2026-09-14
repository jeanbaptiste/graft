package state

import "strings"

// mirrorMarker tags a comment body as graft's own mirrored output, using
// zero-width characters a person can't type by accident — or, short of
// deliberately pasting raw Unicode escapes into a comment box, at all.
// The earlier guard matched on the visible "**via Forgejo:**" etc. prefix
// text itself, which meant a real user typing that same phrase at the
// start of their own comment got it silently excluded from mirroring.
// Low severity (it only suppresses mirroring their own comment, nothing
// escalates), but easy to close: the visible attribution text stays for
// humans reading it, this invisible marker is what the guard actually
// checks.
const mirrorMarker = "​​graft:mirrored​"

// MarkMirrored appends the invisible marker to a comment body that's
// about to be posted as a mirror of content from elsewhere.
//
// Separated from body by a real newline, not appended directly: a body
// ending in a bare URL (a commit link, say) has no visible character
// between it and the marker, so Forgejo's autolinker doesn't stop at the
// URL's actual end — it swallows the zero-width characters and the
// literal text "graft:mirrored" right into the link target, producing a
// dead link (confirmed live: .../commit/<sha> resolves, .../commit/<sha>graft:mirrored
// 404s). A newline is a hard boundary no autolinker crosses.
func MarkMirrored(body string) string {
	return body + "\n" + mirrorMarker
}

// IsMirroredComment reports whether body carries graft's own mirror
// marker — i.e. whether a comment-sync pass produced it, directly or via
// the ActivityPub/AT Proto reply bridges, and so must never be mirrored
// back out.
func IsMirroredComment(body string) bool {
	return strings.Contains(body, mirrorMarker)
}
