# graft

A daemon and web-UI that mirrors repositories between Forgejo instances. Also connect to [Radicle](https://radicle.dev/), Activity Pub and ATProto: git content, issues, comments and pull requests / patches.

It runs on a timer, keeps state in a local SQLite file, and never force-pushes. Real divergences between the two sides are reported and left for humans to resolve.

## What it syncs

- **Git content**: the default branch, fast-forward only.
- **Issues**: Forgejo issues become Radicle issues and vice versa. Create-only for now — edits made after the first mirror aren't propagated yet.
- **Patches / pull requests**: a Forgejo PR opens a Radicle patch (and back), by pushing the PR's head commit to `refs/patches` on the Radicle side, or as a branch + PR on the Forgejo side.
- **Comments**: on issues and patches, both directions, from four origins — native Forgejo ↔ Radicle, a Mastodon reply (ActivityPub), and a Bluesky reply (AT Proto). Dedup'd by content hash, prefixed with where it actually came from. See [TUTORIAL.md § Comments](TUTORIAL.md#comments).

## Why

Forgejo and Radicle solve the same problem — hosting and reviewing changes to a git repository — with opposite architectures: one is a server you point a browser at, the other is a peer-to-peer protocol with no server at all. `graft` lets a project exist on both.

## Dashboard

`graft` serves a status page (`/`) alongside the sync daemon: one row per federation, one cell per mirrored commit/issue/patch/comment, colored by kind. A mirror in sync with the rest of its federation shows the same cells as everyone else, even for content it didn't itself log; a mirror whose last sync pass actually failed shows only what it has confirmed, plus a visible error badge — the heatmap is never a silent source of "why does this one look behind."

Three buttons next to it (`+ Add Forgejo peer`, `+ Add Radicle peer`, `+ New repo`) let a peer join a federation or start a new one without shell access — instantly with the admin password, or as a request the admin approves from `/admin/pending` without one. Each is covered next to its manual equivalent in [TUTORIAL.md](TUTORIAL.md); the shared mechanics are in [§ Self-service forms](TUTORIAL.md#self-service-forms-submitting-and-reviewing).

## Fediverse / AT Proto

Each mirrored series can also speak ActivityPub: `@series@your-host` is followable from Mastodon, posting a note for every commit/issue/patch, with replies bridged back as real comments (see Comments above). A minimal AT Proto client can post the same to Bluesky. Both are off by default — set `public_host` in the config to turn ActivityPub on for a series.

## Requirements

- Go 1.23+ to build
- `git`
- the `rad` CLI, with `git-remote-rad` on `PATH`, and a Radicle node already running with its own identity (`rad auth`)
- a Forgejo access token scoped to `write:repository` and `write:issue` on the repo you're syncing

## Build

```sh
go build -o graft ./cmd/sync
```

## Config

```yaml
sync_interval: 5m
state_db: /var/lib/graft/state.db

repos:
  - name: my-project
    forgejo:
      base_url: https://git.example.org
      owner: alice
      repo: my-project
      token_file: /etc/graft/my-project.token
    radicle:
      rid: rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5
      http_base_url: https://seed.example.org
      rad_home: /home/graft/.radicle
    sync:
      git: true
      issues: true
      patches: true
```

Tokens live in their own files, never inline in the config. `graft` reads them at startup and expects `chmod 600`.

## Run

```sh
graft -config /etc/graft/config.yaml   # loop forever, per sync_interval
graft -config /etc/graft/config.yaml -once   # one pass, then exit
```

A systemd unit is in `systemd/graft.service`.

Full walkthrough, from a blank server to a running mirror: [TUTORIAL.md](TUTORIAL.md).

## License

GPLv3. See [LICENSE](LICENSE).
