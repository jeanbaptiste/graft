// Command sync runs the Forgejo <-> Radicle sync daemon: on an interval, it
// mirrors git content, issues and patches for every repo pair listed in the
// config file, then sleeps until the next pass.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

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
	for _, pair := range cfg.Repos {
		rs, err := sync.New(pair, st, sync.WorkDirFor(stateDir, pair.Name))
		if err != nil {
			log.Error("set up repo pair", "pair", pair.Name, "err", err)
			os.Exit(1)
		}
		syncers = append(syncers, rs)
	}

	tracker := status.NewTracker(st)
	runAll := func() {
		for _, rs := range syncers {
			gitErr, issuesErr, patchErr := rs.Run(log)
			tracker.Record(rs.Name(), gitErr, issuesErr, patchErr)
		}
	}

	if *listen != "" {
		go func() {
			if err := http.ListenAndServe(*listen, tracker.Handler()); err != nil {
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

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
