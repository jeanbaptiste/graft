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
	for _, pair := range cfg.Repos {
		rs, err := sync.New(pair, st, sync.WorkDirFor(stateDir, pair.Name))
		if err != nil {
			log.Error("set up repo pair", "pair", pair.Name, "err", err)
			os.Exit(1)
		}
		syncers = append(syncers, rs)
		syncersByPair[pair.Name] = rs
	}

	topology := buildTopology(cfg)

	tracker := status.NewTracker(st, cfg.SourceURL)
	tracker.SetTopology(topology)
	tracker.SetPublicHost(cfg.PublicHost)

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
			if err := http.ListenAndServe(*listen, mux); err != nil {
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
func buildTopology(cfg *config.Config) map[string][]status.ServerRef {
	topology := map[string][]status.ServerRef{}
	seen := map[string]bool{}
	add := func(series, host, repoURL string) {
		if host == "" {
			return
		}
		key := series + "|" + host
		if seen[key] {
			return
		}
		seen[key] = true
		topology[series] = append(topology[series], status.ServerRef{Label: host, URL: repoURL})
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
		add(series, fHost, fURL)

		rHost := ""
		if u, err := url.Parse(pair.Radicle.HTTPBaseURL); err == nil {
			rHost = u.Host
		}
		add(series, rHost, sync.RadicleExplorerLink(pair.Radicle))
	}
	return topology
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
