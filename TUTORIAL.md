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
chown graft:graft /etc/graft/my-project.token   # the user graft actually runs as, not root
```

### Sharing a token safely

`config.yaml` never holds the token itself, only `token_file`. `graft` requires `token_file` and rejects an inline value. Getting the token into that file is on you:

- Scope it to `write:repository` + `write:issue`. Never admin or org-wide.
- Never paste it into chat, email, or a ticket.
- Prefer the token owner writing it directly into `/etc/graft/<name>.token` over their own SSH session — no third party involved.
- If a third party must relay it, use a one-time-secret tool (Bitwarden Send, 1Password Psst, self-hosted `onetimesecret.com`), not a plain link.
- Rotate it if its exposure is ever in doubt.

See [Built-in token exchange](#built-in-token-exchange) — `graft` has a one-time secret-share page for this.

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

## Adding another repo from the browser

Steps 2–5 above — create the Forgejo repo, mint a token, `rad init`, write the config entry — are also a form, once `graft` itself is already running: the `+ New repo` button next to the dashboard heatmap. Same prerequisites, listed inline on the page (create the Forgejo repo, generate a scoped token, `rad init`, note the RID), then one submission instead of four manual steps. Useful for a second, unrelated project once the first is up; not a way to bootstrap the very first one, since it needs `graft`'s own HTTP server already serving.

Like every self-service form, it has an admin password field that's optional — see [Two ways to submit a self-service form](#two-ways-to-submit-a-self-service-form) for what filling it in, leaving it blank, or getting it wrong each do.

## Adding a peer to an existing federation

Adding a second Forgejo instance to a repo already mirrored between one Forgejo and one Radicle node needs no new Radicle node and no change to the pairs already running.

**From the browser**, this is the `+ Add Forgejo peer` button (`/add-peer`): pick the federation from a dropdown, and the Radicle side (RID, seed, explorer) is filled in for you, not asked for — the manual steps below exist to show what that button does, and to cover the case where the peer's admin doesn't have dashboard access. Same optional-password rule as above.

| Placeholder | Stands for |
|---|---|
| `git2.example.org` | the second Forgejo instance |
| `bob` | the account on it that owns the mirrored repo |

A Radicle RID replicates to every node that seeds it, independent of `graft`. Pointing a second pair's `radicle.http_base_url` at the same seed already in use doesn't create a new relationship — that seed already has the content. No Radicle-side step for this case.

1. On `git2.example.org`, create the repo **without** `auto_init` (`auto_init: false` or omitted) and a token scoped `write:repository` + `write:issue`. `auto_init: true` gives the repo an unrelated first commit; `graft` sees that as a real divergence against the federation's existing history and refuses to push, logging "forgejo and rad diverged since last sync" (see Troubleshooting). An empty repo has no history to diverge from.

   ```sh
   echo "$TOKEN2" > /etc/graft/my-project-git2.token
   chmod 600 /etc/graft/my-project-git2.token
   chown graft:graft /etc/graft/my-project-git2.token
   ```

2. Add a second entry to `config.yaml`, under the same `series` as the first pair (defaults to the pair's own `name` if you never set one — give the *first* pair an explicit `series` too if you didn't already, so both land on the same row):

   ```yaml
   repos:
     - name: my-project-git1
       series: my-project          # both pairs share this
       forgejo:
         base_url: https://git.example.org
         owner: alice
         repo: my-project
         token_file: /etc/graft/my-project.token
       radicle:
         rid: rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5
         http_base_url: https://seed.example.org
         rad_home: /home/graft/.radicle
       sync: { git: true, issues: true, patches: true }

     - name: my-project-git2
       series: my-project          # joins the same federation
       forgejo:
         base_url: https://git2.example.org
         owner: bob
         repo: my-project
         token_file: /etc/graft/my-project-git2.token
       radicle:
         rid: rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5   # same RID — not a new one
         http_base_url: https://seed.example.org   # same seed — not a new node
         rad_home: /home/graft/.radicle             # always graft's own, never the peer's
         explorer_url: https://explorer.example.org
       sync: { git: true, issues: true, patches: true }
   ```

   `rad_home` never changes between pairs — it's `graft`'s own local Radicle identity, the same one it pushes as for every pair it runs. Only the `forgejo` block is specific to the new peer.

3. Restart: `sudo systemctl restart graft`. `config.Load` validates every field on startup — a typo surfaces immediately in the logs, not as a silent no-op.

4. Confirm: the dashboard shows `git2.example.org` as a second row under `my-project`. A side with content but no direct push logged by `graft` shows colored cells instead of a normal commit/issue/patch cell — "source" (content originated here) or "replicated" (arrived via Radicle gossip or a Forgejo Authorized Integration, not a `graft`-logged push). Hover for date, origin, link.

## Adding a brand-new Radicle node to the mesh

Reusing an existing seed needs nothing extra. A node nobody in the mesh has talked to needs two manual steps — `graft`'s own Radicle node doesn't discover new peers on its own:

**From the browser**, this is the `+ Add Radicle peer` button (`/add-radicle-peer`): pick the federation, enter the new node's ID and address, and it runs the same two commands below on `graft`'s own node. Same optional-password rule as [above](#two-ways-to-submit-a-self-service-form).

```sh
rad node connect <new-node-id>@<new-node-address>:8776
rad seed rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5
```

`rad node connect` is the first handshake — Radicle's peer discovery (`"peers": {"type": "dynamic"}` in `~/.radicle/config.json`) spreads pairs it already knows, but a never-connected node needs one explicit introduction. `rad seed` is separate: default seeding policy replicates nothing not explicitly told to. Skip either step and the node stays unreachable or empty.

## Comments

`graft` mirrors comments on issues and patches, both directions, from four origins:

- **Forgejo → Radicle** and **Radicle → Forgejo**: native comments, polled and cross-mirrored each pass. Dedup is by content hash (`comment_seen` table); the mirrored copy is visibly prefixed `**via Forgejo:**` / `**via Radicle:**` for readers, but the actual loop guard is an invisible marker (zero-width characters, `state.MarkMirrored`/`IsMirroredComment`) appended to the body — a real comment that happens to start with the same visible text is never mistaken for one of graft's own mirrors.
- **ActivityPub**: a Mastodon reply to one of graft's posts becomes a comment, prefixed `**via Fediverse, @user:**` (same invisible marker).
- **AT Proto**: a Bluesky reply, same mechanism, prefixed `**via Bluesky, @handle:**`. Not verified against a live account — built from AT Proto's published lexicon (`app.bsky.notification.listNotifications`), but no Bluesky account existed to test against when this was written.

Dashboard cells are colored by kind only (commit, issue, patch, comment) — never by origin. Origin shows up as a tooltip note only, on hover.

## Troubleshooting

**"token does not have at least one of required scope(s)"** — the Forgejo token is missing `write:issue` or `write:repository`. Regenerate it with both.

**"forgejo and rad diverged since last sync"** — both sides got commits since the last pass that aren't ancestors of each other. `graft` won't guess which one wins; merge them by hand on either side and it'll pick up cleanly next pass.

**"'remote-rad' is not a git command"** — `git-remote-rad` (installed alongside `rad`) isn't on `PATH` for the user running `graft`. Add its directory to the systemd unit's `Environment=PATH=...`.

**A patch/PR never shows up on the other side** — check that the corresponding repo isn't still empty on the destination. The first git sync has to land before patches/issues referencing that content can be mirrored.

## Self-service forms: submitting and reviewing

The three buttons next to the federation heatmap — `+ Add Forgejo peer`, `+ Add Radicle peer`, `+ New repo` — are covered in context above, right next to their manual equivalents. This section covers what's common to all three, plus the two footer links:

- **`Share a secret, once`** (`/share/new`, footer) — the one-time-secret page below. Creating a share needs no password; claiming needs the passcode.
- **`Pending requests`** (`/admin/pending`, footer) — review queue, see below.

### Two ways to submit a self-service form

Each of the three onboarding forms has one optional field: the admin password. What happens depends on it:

- **Left blank** — the submission becomes a pending request. Nothing happens yet: no sync, no `rad node connect`, nothing touches any Forgejo or Radicle instance. Anyone can submit one without ever knowing the password — this is the path for someone outside your trust circle (a contributor like Milo) proposing themselves as a peer.
- **Filled in, correct** — goes live immediately, exactly as graft always has: the peer/repo is active within one `sync_interval` (no restart), or for a Radicle peer, `rad node connect` + `rad seed` run right away. This is still the right tool for a class or workshop where you hand the password to a trusted group who should self-serve without a review step.
- **Filled in, wrong** — rejected outright. It never silently falls back to a pending request — that would turn the form into a way to probe the password with no lockout consequence, defeating the rate limiter below.

### Reviewing pending requests

`/admin/pending` is the only place the password gates anything for the request path. It's a stateless review list, no login session: entering the password once renders every pending Forgejo peer, new repo, and Radicle peer request, each with its own Approve/Reject button that re-submits the same password. Approving a Forgejo peer/repo request just flips it active (same "next sync pass" activation as the instant path); approving a Radicle peer request runs `rad node connect` + `rad seed` at that moment. Rejecting a Forgejo peer/repo request also deletes the token file it already wrote to disk, so a rejected submission leaves nothing behind.

Both checks against it (the onboarding forms' optional field, and the pending-review page) use `admin_password` from `config.yaml` (default `graft`; `graft` logs a startup warning if left at that default), constant-time comparison, rate-limited: 5 wrong attempts per IP or 20 total, per 15 minutes. `/share/new`'s claim page is a separate secret entirely — the 6-digit passcode, checked by AES-GCM decryption rather than a stored comparison (see Built-in token exchange below) — with its own, independent rate limiter.

## Built-in token exchange

**Built**, reachable from the dashboard:

- **One-time secret share.** `POST /share/new` encrypts the input with AES-256-GCM, keyed from a passcode and the share's own random id together. Only the id's hash is stored — a database dump alone can't derive the key. `GET/POST /share/<id>` claims it: right passcode reveals it once and deletes the record; 5 wrong passcodes deletes it too; 15 minutes unclaimed and it's gone.
- **QR code.** `/share/<id>/qr.png` encodes the claim link only, not the passcode.

**Not built:**

- **Wormhole-style PAKE exchange** (the protocol behind [`magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)): both sides type the same short code into a CLI; the key exchange derives a shared secret without the code or the token crossing the wire in the clear. `graft`'s server never sees the plaintext token. Most to build — a relay endpoint for the handshake, shell access on both sides. Skipped: the share page above covers the same case at a fraction of the cost.

- **Skip the handoff entirely.** If Forgejo exposes a delegated token-creation flow (peer admin clicks a link, logs into their own Forgejo, a token scoped to exactly `write:repository` + `write:issue` on exactly that repo is minted and handed to `graft` server-to-server), no plaintext secret touches a clipboard, a chat window, or a QR code. Depends on Forgejo's OAuth2/application-token APIs supporting delegated, scope-limited minting driven by a third party — unverified against a current Forgejo instance.
