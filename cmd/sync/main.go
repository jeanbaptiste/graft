// Command sync runs the Forgejo <-> Radicle sync daemon: on an interval, it
// mirrors git content, issues and patches for every repo pair listed in the
// config file, then sleeps until the next pass.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"graft/internal/activitypub"
	"graft/internal/config"
	"graft/internal/state"
	"graft/internal/status"
	"graft/internal/sync"
)

func main() {
	configPath := flag.String("config", "/etc/graft/config.yaml", "path to config.yaml")
	once := flag.Bool("once", false, "run a single pass and exit, instead of looping")
	listen := flag.String("listen", ":8090", "address to serve /healthz and / (status) on; empty disables it")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

	st, err := state.Open(cfg.StateDB)
	if err != nil {
		log.Error("open state db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	stateDir := cfg.StateDB
	if idx := lastSlash(stateDir); idx >= 0 {
		stateDir = stateDir[:idx]
	}

	syncers := make([]*sync.RepoSyncer, 0, len(cfg.Repos))
	syncersByPair := map[string]*sync.RepoSyncer{}
	syncersBySeries := map[string]*sync.RepoSyncer{}
	pairsBySeries := map[string][]*sync.RepoSyncer{}
	for _, pair := range cfg.Repos {
		rs, err := sync.New(pair, st, sync.WorkDirFor(stateDir, pair.Name))
		if err != nil {
			log.Error("set up repo pair", "pair", pair.Name, "err", err)
			os.Exit(1)
		}
		syncers = append(syncers, rs)
		syncersByPair[pair.Name] = rs
		if _, ok := syncersBySeries[rs.Series()]; !ok {
			syncersBySeries[rs.Series()] = rs
		}
		pairsBySeries[rs.Series()] = append(pairsBySeries[rs.Series()], rs)
	}
	detectAuthorizedIntegrations(pairsBySeries, log)

	// aiTargetHosts marks which series+Forgejo-host combinations receive
	// pushes directly via Authorized Integrations rather than via graft
	// itself — read by buildTopology so the dashboard can tell that side's
	// silence apart from a side nothing has ever reached.
	aiTargetHosts := map[string]map[string]bool{}
	for series, pairs := range pairsBySeries {
		for _, rs := range pairs {
			if rs.HasAuthorizedIntegrationSource() {
				if aiTargetHosts[series] == nil {
					aiTargetHosts[series] = map[string]bool{}
				}
				aiTargetHosts[series][rs.ForgejoHost()] = true
			}
		}
	}

	topology := buildTopology(cfg, aiTargetHosts)
	blueskyConfigured := map[string]bool{}
	for _, pair := range cfg.Repos {
		if pair.Bluesky != nil {
			series := pair.Series
			if series == "" {
				series = pair.Name
			}
			blueskyConfigured[series] = true
		}
	}

	tracker := status.NewTracker(st, cfg.SourceURL)
	tracker.SetTopology(topology)
	tracker.SetPublicHost(cfg.PublicHost)
	tracker.SetBlueskyConfigured(blueskyConfigured)

	var apHandler *activitypub.Handler
	if cfg.PublicHost != "" {
		apHandler = activitypub.NewHandler(st, cfg.PublicHost, log,
			func(series string) string {
				if refs := topology[series]; len(refs) > 0 {
					return refs[0].URL
				}
				return ""
			},
			func(series string) bool {
				_, ok := topology[series]
				return ok
			},
			func(repoPair, kind string, forgejoID int64, radicleID, body string) error {
				rs, ok := syncersByPair[repoPair]
				if !ok {
					return fmt.Errorf("unknown repo pair %q", repoPair)
				}
				return rs.CommentOnItem(kind, forgejoID, radicleID, body)
			},
			func(series string) string {
				rs, ok := syncersBySeries[series]
				if !ok {
					return ""
				}
				did, err := rs.RadicleDID()
				if err != nil {
					log.Error("resolve radicle did", "series", series, "err", err)
					return ""
				}
				return did
			},
		)
	}

	runAll := func() {
		for _, rs := range syncers {
			gitErr, issuesErr, patchErr := rs.Run(log)
			tracker.Record(rs.Name(), rs.Series(), gitErr, issuesErr, patchErr)
		}
		if apHandler != nil {
			apHandler.DeliverNewActivity()
		}
	}

	if *listen != "" {
		mux := http.NewServeMux()
		if apHandler != nil {
			apHandler.Register(mux)
		}
		mux.Handle("/", tracker.Handler())

		go func() {
			if err := http.ListenAndServe(*listen, securityHeaders(mux)); err != nil {
				log.Error("status server stopped", "err", err)
			}
		}()
	}

	runAll()
	if *once {
		return
	}

	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	for range ticker.C {
		runAll()
	}
}

// buildTopology declares every mirror target configured for each series
// (its Forgejo side and its Radicle side), so the dashboard can show a
// side that's never produced an event yet — not just sides the activity
// log happens to mention.
func buildTopology(cfg *config.Config, aiTargetHosts map[string]map[string]bool) map[string][]status.ServerRef {
	topology := map[string][]status.ServerRef{}
	seen := map[string]bool{}
	add := func(series, host, repoURL string, radicle, ai bool) {
		if host == "" {
			return
		}
		key := series + "|" + host
		if seen[key] {
			return
		}
		seen[key] = true
		topology[series] = append(topology[series], status.ServerRef{
			Label:                 host,
			URL:                   repoURL,
			Radicle:               radicle,
			AuthorizedIntegration: ai,
		})
	}

	for _, pair := range cfg.Repos {
		series := pair.Series
		if series == "" {
			series = pair.Name
		}

		fHost := ""
		if u, err := url.Parse(pair.Forgejo.BaseURL); err == nil {
			fHost = u.Host
		}
		fURL := strings.TrimRight(pair.Forgejo.BaseURL, "/") + "/" + pair.Forgejo.Owner + "/" + pair.Forgejo.Repo
		add(series, fHost, fURL, false, aiTargetHosts[series][fHost])

		rHost := ""
		if u, err := url.Parse(pair.Radicle.HTTPBaseURL); err == nil {
			rHost = u.Host
		}
		add(series, rHost, sync.RadicleExplorerLink(pair.Radicle), true, false)
	}
	return topology
}

// detectAuthorizedIntegrations checks, for every pair of Forgejo hosts
// sharing a series, whether one side already has a Forgejo Actions
// workflow pushing directly into the other via Authorized Integrations
// (see internal/sync's authint.go) — and if so, tells the receiving
// pair's GitSyncer to stop relaying that content via Radicle itself,
// since the direct path is faster. Run once at startup: workflow files
// don't change often enough to justify re-checking every pass.
func detectAuthorizedIntegrations(pairsBySeries map[string][]*sync.RepoSyncer, log *slog.Logger) {
	for series, pairs := range pairsBySeries {
		if len(pairs) < 2 {
			continue
		}
		for _, source := range pairs {
			for _, dest := range pairs {
				if source == dest {
					continue
				}
				destHost := dest.ForgejoHost()
				if destHost == "" {
					continue
				}
				if sync.HasAuthorizedIntegration(source.ForgejoClient(), destHost) {
					log.Info("authorized integration detected",
						"series", series, "source", source.ForgejoHost(), "target", destHost)
					dest.SetAuthorizedIntegrationSource(source.ForgejoHost())
				}
			}
		}
	}
}

// securityHeaders sets a fixed, unconditional set of hardening response
// headers on every request. Deliberately just static assignments — no
// per-request logic, nothing that can panic or branch incorrectly — since
// this wraps every route graft serves (dashboard, /social, ActivityPub,
// /inbox) and a bug here would affect all of them at once.
//
// CSP is intentionally left out: the dashboard's entire stylesheet is one
// inline <style> block, which a naive CSP would break outright (style-src
// blocks inline styles by default) without a nonce wired through every
// template render — worth doing properly later, not as a blanket header.
// Cross-Origin-Embedder-Policy is left out too: it exists to protect
// cross-origin-isolated contexts (SharedArrayBuffer, WASM threads) that
// graft has no use for, and would only be a trap for a future contributor
// who adds an external resource without thinking to CORS-enable it.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		// 180 days, no includeSubDomains (f1/f2/radicle aren't verified
		// all-HTTPS as a unit) and no preload (effectively irreversible).
		h.Set("Strict-Transport-Security", "max-age=15552000")
		next.ServeHTTP(w, r)
	})
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
