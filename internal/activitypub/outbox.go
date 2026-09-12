package activitypub

import (
	"strconv"
	"time"

	"graft/internal/state"
)

// OrderedCollection wraps a series' outbox: every mirrored event as a
// Create{Note} activity, newest first.
type OrderedCollection struct {
	Context      string   `json:"@context"`
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	TotalItems   int      `json:"totalItems"`
	OrderedItems []Create `json:"orderedItems"`
}

type Create struct {
	Context   string   `json:"@context,omitempty"`
	ID        string   `json:"id"`
	Type      string   `json:"type"`
	Actor     string   `json:"actor"`
	Published string   `json:"published"`
	To        []string `json:"to,omitempty"`
	Object    Note     `json:"object"`
}

// Note is one mirrored event, addressable on its own
// (/actors/{series}/notes/{id}) so a reply's inReplyTo can point back at
// the specific commit/issue/patch it's about.
type Note struct {
	Context      string   `json:"@context,omitempty"`
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	AttributedTo string   `json:"attributedTo"`
	Content      string   `json:"content"`
	URL          string   `json:"url,omitempty"`
	Published    string   `json:"published"`
	To           []string `json:"to,omitempty"`
}

const publicAudience = "https://www.w3.org/ns/activitystreams#Public"

// NoteURI returns the stable object URI for one activity_log entry's Note.
func NoteURI(host, series string, entryID int64) string {
	return ActorURI(host, series) + "/notes/" + strconv.FormatInt(entryID, 10)
}

// BuildNote turns one mirrored event into its Note representation.
func BuildNote(host, series string, e state.ActivityEntry) Note {
	actor := ActorURI(host, series)
	content := kindLabel(e.Kind) + ": " + e.Summary
	return Note{
		ID:           NoteURI(host, series, e.ID),
		Type:         "Note",
		AttributedTo: actor,
		Content:      content,
		URL:          e.URL,
		Published:    e.OccurredAt.Format(time.RFC3339),
		To:           []string{publicAudience},
	}
}

// BuildOutbox wraps a series' recent activity entries (newest first, as
// returned by state.Store.ActivityForSeries) into an OrderedCollection of
// Create activities.
func BuildOutbox(host, series string, entries []state.ActivityEntry) OrderedCollection {
	base := ActorURI(host, series)
	actor := base
	items := make([]Create, 0, len(entries))
	for _, e := range entries {
		note := BuildNote(host, series, e)
		items = append(items, Create{
			ID:        note.ID + "/activity",
			Type:      "Create",
			Actor:     actor,
			Published: note.Published,
			To:        []string{publicAudience},
			Object:    note,
		})
	}
	return OrderedCollection{
		Context:      "https://www.w3.org/ns/activitystreams",
		ID:           base + "/outbox",
		Type:         "OrderedCollection",
		TotalItems:   len(items),
		OrderedItems: items,
	}
}

func kindLabel(kind string) string {
	switch kind {
	case "git":
		return "Commit"
	case "issue":
		return "Issue"
	case "patch":
		return "Patch"
	default:
		return kind
	}
}
