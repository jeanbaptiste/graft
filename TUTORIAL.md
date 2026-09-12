# Tutorial

This walks through mirroring one repository between a Forgejo instance and a Radicle node, from nothing. Replace the names below with your own:

| Placeholder | Stands for |
|---|---|
| `git.example.org` | your Forgejo instance |
| `seed.example.org` | your Radicle node's `radicle-httpd` address |
| `alice` | the Forgejo account that owns the repo |
| `my-project` | the repository |

## Prerequisites

- A Forgejo instance you can create repos and tokens on.
- A machine with `git`, the `rad` CLI, and a running `radicle-node` — see [Radicle's own seeder guide](https://radicle.dev/guides/seeder) if you don't have one yet.
- Go 1.23+, to build `graft`.

## 1. Build graft

```sh
git clone https://git.example.org/alice/graft.git
cd graft
go build -o graft ./cmd/sync
sudo mv graft /usr/local/bin/graft
```

## 2. Create a Forgejo token

`graft` needs a token scoped to `write:repository` and `write:issue` on the repo it syncs.

```sh
curl -X POST -H "Authorization: token $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"graft","scopes":["write:repository","write:issue"]}' \
  https://git.example.org/api/v1/users/alice/tokens
```

Save the returned token somewhere `graft` can read and nowhere else can:

```sh
mkdir -p /etc/graft
echo "$TOKEN" > /etc/graft/my-project.token
chmod 600 /etc/graft/my-project.token
```

## 3. Create the repo on Forgejo

```sh
curl -X POST -H "Authorization: token $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-project","auto_init":true,"default_branch":"main"}' \
  https://git.example.org/api/v1/user/repos
```

`auto_init` gives it one commit, so there's something to mirror.

## 4. Create the repo on Radicle

Radicle repos are created from an existing git checkout. Clone the one you just made and initialize it:

```sh
git clone https://git.example.org/alice/my-project.git
cd my-project
rad init --name my-project --default-branch main --public
```

This prints a Repository ID (RID), something like `rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5`. Write it down — the config needs it.

If you have other Radicle nodes that should also carry this repo, tell them to seed it:

```sh
rad seed rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5
```

## 5. Write the config

```yaml
# /etc/graft/config.yaml
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

`rad_home` is the `RAD_HOME` of the identity `graft` pushes as — usually a dedicated one, created with `rad auth` under whatever user runs `graft`.

## 6. Run it once

```sh
mkdir -p /var/lib/graft
graft -config /etc/graft/config.yaml -once
```

No output means no errors. Check both sides:

```sh
curl -s https://seed.example.org/api/v1/repos/rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5
```

If you open an issue on the Forgejo repo and run `graft -once` again, it shows up on Radicle within that one pass.

## 7. Run it as a service

```sh
sudo cp systemd/graft.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now graft
```

It now mirrors both repos every `sync_interval`, in both directions, on its own.

## Troubleshooting

**"token does not have at least one of required scope(s)"** — the Forgejo token is missing `write:issue` or `write:repository`. Regenerate it with both.

**"forgejo and rad diverged since last sync"** — both sides got commits since the last pass that aren't ancestors of each other. `graft` won't guess which one wins; merge them by hand on either side and it'll pick up cleanly next pass.

**"'remote-rad' is not a git command"** — `git-remote-rad` (installed alongside `rad`) isn't on `PATH` for the user running `graft`. Add its directory to the systemd unit's `Environment=PATH=...`.

**A patch/PR never shows up on the other side** — check that the corresponding repo isn't still empty on the destination. The first git sync has to land before patches/issues referencing that content can be mirrored.
