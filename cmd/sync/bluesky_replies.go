package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"graft/internal/atproto"
	"graft/internal/config"
	"graft/internal/state"
)

// blueskyPollInterval throttles how often pollSeriesReplies actually logs
// in per series, independent of the main sync loop's own interval.
// pollBlueskyReplies is called on every runAll() tick — with
// sync_interval set as low as 1m (as it is in production), calling
// pollSeriesReplies unthrottled logs in fresh, on every single tick, for
// every series with Bluesky configured. That hammered this PDS's own
// createSession endpoint hard enough to trip its rate limiter (HTTP 429)
// within about ten minutes of going live — discovered directly from
// graft's own logs, not simulated.
const blueskyPollInterval = 5 * time.Minute

var (
	blueskyLastPollMu sync.Mutex
	blueskyLastPoll   = map[string]time.Time{}
)

func blueskyShouldPoll(series string) bool {
	blueskyLastPollMu.Lock()
	defer blueskyLastPollMu.Unlock()
	if t, ok := blueskyLastPoll[series]; ok && time.Since(t) < blueskyPollInterval {
		return false
	}
	blueskyLastPoll[series] = time.Now()
	return true
}

// pollBlueskyReplies checks every series with a Bluesky account configured
// for new replies to posts graft itself made, and bridges each one back
// onto the underlying Forgejo/Radicle item — the AT Proto equivalent of
// activitypub's handleReply.
//
// NOT verified against a live Bluesky account: none was configured in
// this deployment at the time this was written (see BlueskyTarget in
// config.go — a nil block is a no-op, so this simply never runs until
// one exists). Built directly from AT Proto's published
// app.bsky.notification.listNotifications / app.bsky.feed.post lexicon.
// Verify against a real account and a real reply before trusting this
// deeply — see atproto.Notification's doc comment.
func pollBlueskyReplies(pairs []config.RepoPair, live *liveState, st *state.Store, log *slog.Logger) {
	seenSeries := map[string]bool{}
	for _, pair := range pairs {
		if pair.Bluesky == nil {
			continue
		}
		series := pair.Series
		if series == "" {
			series = pair.Name
		}
		if seenSeries[series] {
			continue
		}
		seenSeries[series] = true

		if !blueskyShouldPoll(series) {
			continue
		}

		appPassword, err := config.ReadToken(pair.Bluesky.AppPasswordFile)
		if err != nil {
			log.Error("bluesky reply poll: read app password", "series", series, "err", err)
			continue
		}
		if err := pollSeriesReplies(series, pair.Bluesky.Handle, appPassword, pair.Bluesky.PDSHost, live, st, log); err != nil {
			log.Error("bluesky reply poll", "series", series, "err", err)
		}
	}
}

func pollSeriesReplies(series, handle, appPassword, pdsHost string, live *liveState, st *state.Store, log *slog.Logger) error {
	c := atproto.New(pdsHost)
	if err := c.Login(handle, appPassword); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	notifications, err := c.ListNotifications()
	if err != nil {
		return fmt.Errorf("list notifications: %w", err)
	}

	watermark, err := st.ATProtoCursor(series)
	if err != nil {
		return fmt.Errorf("read cursor: %w", err)
	}
	newest := watermark

	// Newest first per the API's convention; process oldest-first so the
	// watermark advances monotonically even if this loop is interrupted
	// partway through.
	for i := len(notifications) - 1; i >= 0; i-- {
		n := notifications[i]
		if n.Reason != "reply" || n.Record.Reply == nil {
			continue
		}
		if watermark != "" && n.IndexedAt <= watermark {
			continue
		}
		if n.IndexedAt > newest {
			newest = n.IndexedAt
		}

		parentURI := n.Record.Reply.Parent.URI
		activityID, found, err := st.ATProtoPostActivity(parentURI)
		if err != nil {
			log.Error("bluesky reply: lookup parent", "series", series, "err", err)
			continue
		}
		if !found {
			// Not a post graft made about a commit or patch — but if the
			// series' own account wrote it (an announcement, a question to
			// followers), the reply is still discussion about this
			// repository: record it on the wiki with that post as its
			// subject. Replies to anyone else's posts stay on Bluesky.
			if !strings.HasPrefix(parentURI, "at://"+c.DID()+"/app.bsky.feed.post/") {
				continue
			}
			rs, ok := live.syncerForSeries(series)
			if !ok {
				continue
			}
			title := "post Bluesky de @" + handle
			if text, err := c.PostText(parentURI); err == nil && strings.TrimSpace(text) != "" {
				title = truncateATProtoSummary(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
			}
			if err := rs.LogSocialReply("Bluesky", "@"+n.Author.Handle, strings.TrimSpace(n.Record.Text), "post", title,
				blueskyPermalink(handle, parentURI), blueskyPermalink(n.Author.Handle, n.URI), 0, "", time.Now()); err != nil {
				log.Error("bluesky reply: log reply to account post", "series", series, "err", err)
			}
			continue
		}
		e, err := st.ActivityByID(activityID)
		if err != nil {
			log.Error("bluesky reply: load activity", "series", series, "id", activityID, "err", err)
			continue
		}
		// Replies to a commit's post open (or continue) that commit's
		// discussion issue — see RepoSyncer.LogSocialReply.
		if e == nil || (e.Kind != "issue" && e.Kind != "patch" && e.Kind != "git") {
			continue
		}
		rs, ok := live.syncerForPair(e.RepoPair)
		if !ok {
			continue
		}

		itemTitle := e.Summary
		if itemTitle == "" {
			itemTitle = e.RepoPair
		}
		sourceURL := blueskyPermalink(n.Author.Handle, n.URI)
		if err := logSocialReplyRouted(live, rs, "Bluesky", "@"+n.Author.Handle, strings.TrimSpace(n.Record.Text), e.Kind, itemTitle, e.URL, sourceURL, e.ForgejoID, e.RadicleID); err != nil {
			log.Error("bluesky reply: log social reply", "series", series, "repo_pair", e.RepoPair, "err", err)
			continue
		}
	}

	if newest != watermark {
		if err := st.SetATProtoCursor(series, newest); err != nil {
			return fmt.Errorf("save cursor: %w", err)
		}
	}
	return nil
}

// blueskyPermalink builds the human-browsable bsky.app URL for a reply
// post from its own AT-URI (at://did/app.bsky.feed.post/rkey) and its
// author's handle — the trackback link shown next to the reply on
// graft's wiki page. Falls back to empty (no trackback shown, not a
// broken link) if the URI isn't the expected shape rather than guessing.
func blueskyPermalink(handle, atURI string) string {
	const want = "app.bsky.feed.post/"
	i := strings.LastIndex(atURI, want)
	if i < 0 || handle == "" {
		return ""
	}
	rkey := atURI[i+len(want):]
	if rkey == "" {
		return ""
	}
	return fmt.Sprintf("https://bsky.app/profile/%s/post/%s", handle, rkey)
}

func truncateATProtoSummary(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
