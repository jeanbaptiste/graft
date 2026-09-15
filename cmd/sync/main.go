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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"graft/internal/activitypub"
	"graft/internal/admin"
	"graft/internal/config"
	"graft/internal/state"
	"graft/internal/status"
	gsync "graft/internal/sync"
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
	if cfg.AdminPassword == "graft" {
		log.Warn("admin_password is left at its default value — anyone who knows it can add a peer or start a repo; set admin_password in config.yaml to change it")
	}

	st, err := state.Open(cfg.StateDB)
	if err != nil {
		log.Error("open state db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	peersFile := cfg.PeersFile
	if peersFile == "" {
		peersFile = filepath.Join(filepath.Dir(*configPath), "peers.yaml")
	}
	// Before anything reads dynamic_repo: a peer the peers file knows but
	// state.db lost (restored from an old backup, or recreated empty)
	// comes back here instead of silently vanishing from the federation.
	if restored, err := st.ImportDynamicRepos(peersFile); err != nil {
		log.Error("import peers file", "path", peersFile, "err", err)
	} else if len(restored) > 0 {
		log.Warn("restored onboarded peers missing from state db", "path", peersFile, "peers", restored)
	}

	stateDir := cfg.StateDB
	if idx := lastSlash(stateDir); idx >= 0 {
		stateDir = stateDir[:idx]
	}
	tokenDir := stateDir
	if len(cfg.Repos) > 0 {
		if idx := lastSlash(cfg.Repos[0].Forgejo.TokenFile); idx >= 0 {
			tokenDir = cfg.Repos[0].Forgejo.TokenFile[:idx]
		}
	}
	defaultRadHome := ""
	for _, p := range cfg.Repos {
		if p.Radicle != nil {
			defaultRadHome = p.Radicle.RadHome
			break
		}
	}

	// live holds every piece of runtime state a dynamically-added peer or
	// repo needs to join without a restart — mutated only from the
	// runAll/ticker goroutine, but read concurrently by HTTP handler
	// goroutines (ActivityPub callbacks, the admin package's Series
	// lookup), so every access goes through mu.
	live := &liveState{
		pairs:           append([]config.RepoPair(nil), cfg.Repos...),
		syncersByPair:   map[string]*gsync.RepoSyncer{},
		syncersBySeries: map[string]*gsync.RepoSyncer{},
		pairsBySeries:   map[string][]*gsync.RepoSyncer{},
	}
	if err := checkPairCollisions(cfg.Repos, st); err != nil {
		log.Error("pair collision check", "err", err)
		os.Exit(1)
	}

	for _, pair := range cfg.Repos {
		rs, err := gsync.New(pair, st, gsync.WorkDirFor(stateDir, pair.Name))
		if err != nil {
			log.Error("set up repo pair", "pair", pair.Name, "err", err)
			os.Exit(1)
		}
		live.addSyncer(rs)
	}
	// dynamic_repo rows from a previous run persist in the database, but
	// live.syncers starts empty every process start regardless of each
	// row's "materialized" flag — that flag only means "graft has synced
	// this at least once ever", not "this process already has a live
	// RepoSyncer for it". Load all of them now, not just ones still
	// pending their first sync.
	loadAllDynamicRepos(st, live, stateDir, log)
	exportPeers(st, peersFile, log)
	detectAuthorizedIntegrations(live.pairsBySeriesSnapshot(), log)
	live.rebuildTopology(cfg)

	tracker := status.NewTracker(st, cfg.SourceURL)
	tracker.SetTopology(live.topologySnapshot())
	tracker.SetPublicHost(cfg.PublicHost)
	tracker.SetBlueskyConfigured(blueskyConfigured(cfg.Repos))

	var apHandler *activitypub.Handler
	if cfg.PublicHost != "" {
		apHandler = activitypub.NewHandler(st, cfg.PublicHost, log,
			func(series string) string {
				if refs := live.topologySnapshot()[series]; len(refs) > 0 {
					return refs[0].URL
				}
				return ""
			},
			func(series string) bool {
				_, ok := live.topologySnapshot()[series]
				return ok
			},
			func(repoPair, platform, author, body, itemKind, itemTitle, itemURL, sourceURL string, forgejoID int64, radicleID string) error {
				log.Info("postSocial: entered", "repo_pair", repoPair, "platform", platform)
				rs, ok := live.syncerForPair(repoPair)
				log.Info("postSocial: syncerForPair returned", "repo_pair", repoPair, "found", ok)
				if !ok {
					return fmt.Errorf("unknown repo pair %q", repoPair)
				}
				err := rs.LogSocialReply(platform, author, body, itemKind, itemTitle, itemURL, sourceURL, forgejoID, radicleID, time.Now())
				log.Info("postSocial: LogSocialReply returned", "repo_pair", repoPair, "err", err)
				return err
			},
			func(series string) string {
				rs, ok := live.syncerForSeries(series)
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

	adminHandler := admin.NewHandler(admin.Config{
		Store:          st,
		AdminPassword:  cfg.AdminPassword,
		PublicHost:     cfg.PublicHost,
		TokenDir:       tokenDir,
		DefaultRadHome: defaultRadHome,
		Series:         live.seriesInfos,
	})

	// lastRun lets one fast ticker drive every pair while pairs on a
	// host_sync_intervals host still only run at their own, slower pace.
	// Only touched from the runAll goroutine.
	lastRun := map[string]time.Time{}

	runAll := func() {
		exportPeers(st, peersFile, log)
		if materializeDynamicRepos(st, live, stateDir, log) {
			detectAuthorizedIntegrations(live.pairsBySeriesSnapshot(), log)
			live.rebuildTopology(cfg)
			tracker.SetTopology(live.topologySnapshot())
			tracker.SetBlueskyConfigured(blueskyConfigured(live.pairsSnapshot()))
		}
		pairsByName := map[string]config.RepoPair{}
		for _, p := range live.pairsSnapshot() {
			pairsByName[p.Name] = p
		}
		for _, rs := range live.syncersSnapshot() {
			if p, ok := pairsByName[rs.Name()]; ok {
				// A second of slack so a pass that starts a hair early
				// on the next tick isn't skipped for a whole interval.
				if last, ran := lastRun[rs.Name()]; ran && time.Since(last) < cfg.IntervalFor(p)-time.Second {
					continue
				}
			}
			lastRun[rs.Name()] = time.Now()
			gitErr, issuesErr, patchErr := rs.Run(log)
			tracker.Record(rs.Name(), rs.Series(), gitErr, issuesErr, patchErr)
		}
		if apHandler != nil {
			apHandler.DeliverNewActivity()
		}
		pollBlueskyReplies(live.pairsSnapshot(), live, st, log)
	}

	if *listen != "" {
		mux := http.NewServeMux()
		if apHandler != nil {
			apHandler.Register(mux)
		}
		adminHandler.Register(mux)
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

// liveState is every piece of runtime registration that can grow after
// startup (a peer or repo added through the dashboard), guarded by one
// mutex since it's written from the ticker goroutine and read from HTTP
// handler goroutines.
type liveState struct {
	mu              sync.Mutex
	pairs           []config.RepoPair
	syncers         []*gsync.RepoSyncer
	syncersByPair   map[string]*gsync.RepoSyncer
	syncersBySeries map[string]*gsync.RepoSyncer
	pairsBySeries   map[string][]*gsync.RepoSyncer
	topology        map[string][]status.ServerRef
}

func (l *liveState) addSyncer(rs *gsync.RepoSyncer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncers = append(l.syncers, rs)
	l.syncersByPair[rs.Name()] = rs
	if _, ok := l.syncersBySeries[rs.Series()]; !ok {
		l.syncersBySeries[rs.Series()] = rs
	}
	l.pairsBySeries[rs.Series()] = append(l.pairsBySeries[rs.Series()], rs)
}

func (l *liveState) addPair(p config.RepoPair) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pairs = append(l.pairs, p)
}

func (l *liveState) syncersSnapshot() []*gsync.RepoSyncer {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*gsync.RepoSyncer(nil), l.syncers...)
}

func (l *liveState) pairsSnapshot() []config.RepoPair {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]config.RepoPair(nil), l.pairs...)
}

func (l *liveState) pairsBySeriesSnapshot() map[string][]*gsync.RepoSyncer {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string][]*gsync.RepoSyncer, len(l.pairsBySeries))
	for k, v := range l.pairsBySeries {
		out[k] = append([]*gsync.RepoSyncer(nil), v...)
	}
	return out
}

func (l *liveState) syncerForPair(name string) (*gsync.RepoSyncer, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rs, ok := l.syncersByPair[name]
	return rs, ok
}

func (l *liveState) syncerForSeries(series string) (*gsync.RepoSyncer, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rs, ok := l.syncersBySeries[series]
	return rs, ok
}

func (l *liveState) topologySnapshot() map[string][]status.ServerRef {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.topology
}

// rebuildTopology recomputes the dashboard's topology map from the
// current set of pairs (static config plus anything added dynamically)
// and the AI-detection state each RepoSyncer already carries.
func (l *liveState) rebuildTopology(cfg *config.Config) {
	l.mu.Lock()
	pairs := append([]config.RepoPair(nil), l.pairs...)
	pairsBySeries := make(map[string][]*gsync.RepoSyncer, len(l.pairsBySeries))
	for k, v := range l.pairsBySeries {
		pairsBySeries[k] = v
	}
	l.mu.Unlock()

	aiTargetHosts := map[string]map[string]bool{}
	for series, rss := range pairsBySeries {
		for _, rs := range rss {
			if rs.HasAuthorizedIntegrationSource() {
				if aiTargetHosts[series] == nil {
					aiTargetHosts[series] = map[string]bool{}
				}
				aiTargetHosts[series][rs.ForgejoHost()] = true
			}
		}
	}
	topo := buildTopology(pairs, aiTargetHosts)

	l.mu.Lock()
	l.topology = topo
	l.mu.Unlock()
}

// seriesInfos is the admin package's view of every federation a new peer
// can join — one entry per series, carrying whichever pair's Radicle side
// every sibling in that series already shares.
func (l *liveState) seriesInfos() []admin.SeriesInfo {
	pairs := l.pairsSnapshot()
	seen := map[string]admin.SeriesInfo{}
	var order []string
	for _, p := range pairs {
		series := p.Series
		if series == "" {
			series = p.Name
		}
		if _, ok := seen[series]; !ok {
			info := admin.SeriesInfo{Name: series}
			if p.Radicle != nil {
				info.RadicleRID = p.Radicle.RID
				info.RadicleHTTPBaseURL = p.Radicle.HTTPBaseURL
				info.RadicleExplorerURL = p.Radicle.ExplorerURL
			}
			seen[series] = info
			order = append(order, series)
		}
	}
	sort.Strings(order)
	out := make([]admin.SeriesInfo, 0, len(order))
	for _, s := range order {
		out = append(out, seen[s])
	}
	return out
}

// checkPairCollisions refuses to start if any single Forgejo repository
// (identified by base_url+owner/repo, case-sensitive — Forgejo repo paths
// are) is claimed as a sync target by more than one pair, whether that
// pair lives in config.yaml or was added later through the dashboard's
// self-service onboarding (dynamic_repo, invisible in this file). Two
// independent pairs mirroring into the same repo don't know about each
// other's mirrored content and will treat it as newly authored, each
// re-mirroring what the other just created — an amplifying duplication
// loop, not a one-off. Confirmed live: adding a second, static
// Forgejo<->Forgejo pair for a series that already had a working
// dynamic Forgejo<->Radicle pair into the very same repo produced 36
// duplicate issues across two repos in under two minutes before the
// daemon was stopped by hand. See docs/PAIRING.md for the two ways a
// pair can exist and why this check can't just live in config.go's
// validate() (it has no DB access, and dynamic_repo rows aren't known
// until runtime).
func checkPairCollisions(staticPairs []config.RepoPair, st *state.Store) error {
	claimedBy := map[string]string{} // target key -> pair name that claims it

	claim := func(target config.ForgejoTarget, pairName string) error {
		if target.BaseURL == "" {
			return nil
		}
		key := strings.TrimRight(target.BaseURL, "/") + "/" + target.Owner + "/" + target.Repo
		if existing, ok := claimedBy[key]; ok && existing != pairName {
			return fmt.Errorf(
				"%s is a sync target for both %q and %q — two independent pairs mirroring into the same repo will duplicate each other's content (see docs/PAIRING.md); one must be config.yaml-static and the other must be found via the dashboard's dynamic peer list and removed, or the reverse",
				key, existing, pairName)
		}
		claimedBy[key] = pairName
		return nil
	}

	for _, p := range staticPairs {
		if err := claim(p.Forgejo, p.Name); err != nil {
			return err
		}
		if p.ForgejoMirror != nil {
			if err := claim(*p.ForgejoMirror, p.Name); err != nil {
				return err
			}
		}
	}

	dynamics, err := st.AllDynamicRepos()
	if err != nil {
		return fmt.Errorf("list dynamic repos for collision check: %w", err)
	}
	for _, d := range dynamics {
		target := config.ForgejoTarget{BaseURL: d.ForgejoBaseURL, Owner: d.ForgejoOwner, Repo: d.ForgejoRepo}
		if err := claim(target, d.Name); err != nil {
			return err
		}
	}
	return nil
}

func dynamicRepoToPair(d state.DynamicRepo) config.RepoPair {
	return config.RepoPair{
		Name:   d.Name,
		Series: d.Series,
		Forgejo: config.ForgejoTarget{
			BaseURL: d.ForgejoBaseURL, Owner: d.ForgejoOwner, Repo: d.ForgejoRepo, TokenFile: d.ForgejoTokenFile,
		},
		Radicle: &config.RadicleTarget{
			RID: d.RadicleRID, HTTPBaseURL: d.RadicleHTTPBaseURL, RadHome: d.RadicleRadHome, ExplorerURL: d.RadicleExplorerURL,
		},
		Sync: config.SyncScope{Git: d.SyncGit, Issues: d.SyncIssues, Patches: d.SyncPatches},
	}
}

// loadAllDynamicRepos rebuilds live RepoSyncers for every dynamic_repo row
// ever created, materialized or not — called once at startup, since
// live.syncers is always empty at process start no matter what the
// database's "materialized" flag says. Deliberately non-fatal (unlike a
// bad static config.yaml pair, which does exit): the whole point of the
// self-service onboarding path is resilience to transient failures (the
// peer's Forgejo temporarily unreachable, say) — refusing to start graft
// entirely over one dynamic peer would be a worse outcome than the bug
// this function fixes. A row that fails here just stays out of the sync
// loop until the next successful restart.
func loadAllDynamicRepos(st *state.Store, live *liveState, stateDir string, log *slog.Logger) {
	rows, err := st.AllDynamicRepos()
	if err != nil {
		log.Error("list dynamic repos", "err", err)
		return
	}
	for _, d := range rows {
		pair := dynamicRepoToPair(d)
		rs, err := gsync.New(pair, st, gsync.WorkDirFor(stateDir, pair.Name))
		if err != nil {
			log.Error("load dynamic repo", "name", d.Name, "err", err)
			continue
		}
		live.addSyncer(rs)
		live.addPair(pair)
		log.Info("loaded dynamic repo", "name", d.Name, "series", d.Series)
	}
}

// materializeDynamicRepos turns every dynamic_repo row nobody has
// activated yet into a live RepoSyncer, joining the sync loop on this
// very pass — no restart, matching the "shows up next pass" model the
// rest of graft already uses. Returns whether anything changed, so the
// caller only pays for a topology rebuild when it's actually needed.
func materializeDynamicRepos(st *state.Store, live *liveState, stateDir string, log *slog.Logger) bool {
	rows, err := st.UnmaterializedDynamicRepos()
	if err != nil {
		log.Error("list dynamic repos", "err", err)
		return false
	}
	changed := false
	for _, d := range rows {
		pair := dynamicRepoToPair(d)
		rs, err := gsync.New(pair, st, gsync.WorkDirFor(stateDir, pair.Name))
		if err != nil {
			// Left unmaterialized — retried next pass rather than
			// dropped, in case the failure is transient (e.g. the
			// Forgejo repo wasn't reachable yet).
			log.Error("materialize dynamic repo", "name", d.Name, "err", err)
			continue
		}
		live.addSyncer(rs)
		live.addPair(pair)
		if err := st.MarkDynamicRepoMaterialized(d.ID); err != nil {
			log.Error("mark dynamic repo materialized", "name", d.Name, "err", err)
		}
		log.Info("materialized dynamic repo", "name", d.Name, "series", d.Series)
		changed = true
	}
	return changed
}

// exportPeers keeps the peers file in step with dynamic_repo. Called at
// startup and every pass, so an approval from /admin/pending reaches the
// file within one sync interval; a no-op when nothing changed.
func exportPeers(st *state.Store, path string, log *slog.Logger) {
	changed, err := st.ExportDynamicRepos(path)
	if err != nil {
		log.Error("export peers file", "path", path, "err", err)
		return
	}
	if changed {
		log.Info("peers file updated", "path", path)
	}
}

// blueskyConfigured maps each series to its AT Proto handle, for the
// /social page's status badge and profile link ("" = not configured).
func blueskyConfigured(repos []config.RepoPair) map[string]string {
	out := map[string]string{}
	for _, pair := range repos {
		if pair.Bluesky != nil {
			series := pair.Series
			if series == "" {
				series = pair.Name
			}
			out[series] = pair.Bluesky.Handle
		}
	}
	return out
}

// buildTopology declares every mirror target configured for each series
// (its Forgejo side and its Radicle side), so the dashboard can show a
// side that's never produced an event yet — not just sides the activity
// log happens to mention.
func buildTopology(pairs []config.RepoPair, aiTargetHosts map[string]map[string]bool) map[string][]status.ServerRef {
	topology := map[string][]status.ServerRef{}
	seen := map[string]bool{}
	add := func(series, host, repoURL, pairName string, radicle, ai bool) {
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
			PairName:              pairName,
		})
	}

	for _, pair := range pairs {
		series := pair.Series
		if series == "" {
			series = pair.Name
		}

		fHost := ""
		if u, err := url.Parse(pair.Forgejo.BaseURL); err == nil {
			fHost = u.Host
		}
		fURL := strings.TrimRight(pair.Forgejo.BaseURL, "/") + "/" + pair.Forgejo.Owner + "/" + pair.Forgejo.Repo
		add(series, fHost, fURL, pair.Name, false, aiTargetHosts[series][fHost])

		if pair.Radicle != nil {
			rHost := ""
			if u, err := url.Parse(pair.Radicle.HTTPBaseURL); err == nil {
				rHost = u.Host
			}
			add(series, rHost, gsync.RadicleExplorerLink(*pair.Radicle), pair.Name, true, false)
		}
		if pair.ForgejoMirror != nil {
			mHost := ""
			if u, err := url.Parse(pair.ForgejoMirror.BaseURL); err == nil {
				mHost = u.Host
			}
			mURL := strings.TrimRight(pair.ForgejoMirror.BaseURL, "/") + "/" + pair.ForgejoMirror.Owner + "/" + pair.ForgejoMirror.Repo
			add(series, mHost, mURL, pair.Name, false, aiTargetHosts[series][mHost])
		}
	}
	return topology
}

// detectAuthorizedIntegrations checks, for every pair of Forgejo hosts
// sharing a series, whether one side already has a Forgejo Actions
// workflow pushing directly into the other via Authorized Integrations
// (see internal/sync's authint.go) — and if so, tells the receiving
// pair's GitSyncer to stop relaying that content via Radicle itself,
// since the direct path is faster. Run at startup and again whenever a
// new peer joins dynamically — workflow files don't change often enough
// to justify checking on every ordinary sync pass.
func detectAuthorizedIntegrations(pairsBySeries map[string][]*gsync.RepoSyncer, log *slog.Logger) {
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
				if gsync.HasAuthorizedIntegration(source.ForgejoClient(), destHost) {
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
// /inbox, the admin onboarding forms) and a bug here would affect all of
// them at once.
//
// Cross-Origin-Embedder-Policy is deliberately left out: it exists to
// protect cross-origin-isolated contexts (SharedArrayBuffer, WASM threads)
// that graft has no use for, and would only be a trap for a future
// contributor who adds an external resource without thinking to
// CORS-enable it.
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
		// Tested locally (Playwright, real dashboard/social HTML, with a
		// negative control proving the test actually catches breakage)
		// before shipping: default-src 'none' blocks all script execution
		// (the real XSS mitigation graft gets from this) without touching
		// rendering, since style-src 'unsafe-inline' covers the one inline
		// <style> block each page has — nothing else is ever injected into
		// it, so there's no practical downside to allowing it specifically.
		// form-action 'self': the admin onboarding/share pages are plain
		// HTML forms posting back to graft itself — 'none' would silently
		// block every one of them.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
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
