# graft

A daemon that mirrors repositories between Forgejo and Radicle: git content, issues, and pull requests / patches.

It runs on a timer, keeps state in a local SQLite file, and never force-pushes. Real divergences between the two sides are reported and left for humans to resolve.

## What it syncs

- **Git content**: the default branch, fast-forward only.
- **Issues**: Forgejo issues become Radicle issues and vice versa. Create-only for now — edits made after the first mirror aren't propagated yet.
- **Patches / pull requests**: a Forgejo PR opens a Radicle patch (and back), by pushing the PR's head commit to `refs/patches` on the Radicle side, or as a branch + PR on the Forgejo side.

## Planned: Forgejo-to-Forgejo via Authorized Integrations

Right now, two Forgejo instances stay in sync by both pairing with the same Radicle repository — Radicle is the hub. An alternative worth supporting directly: one instance's CI authenticates straight to another's API using [Forgejo's Authorized Integrations](https://forgejo.org) (a short-lived OIDC token, no stored secret), instead of routing through Radicle at all. Not implemented yet — noted here so the config shape leaves room for it.

## Why

Forgejo and Radicle solve the same problem — hosting and reviewing changes to a git repository — with opposite architectures: one is a server you point a browser at, the other is a peer-to-peer protocol with no server at all. `graft` lets a project exist on both.

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
