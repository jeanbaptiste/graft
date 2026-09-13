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

The token never belongs in `config.yaml` — only its file path does (`token_file`). The token itself lives in its own `chmod 600` file, owned by the user `graft` runs as. That much is enforced by `graft` itself (`config.Load` requires `token_file`, never accepts an inline value). Getting the token *into* that file safely is on you — a few rules:

- **Scope it down.** `write:repository` + `write:issue` only — never an admin or org-wide token. A leaked scoped token can only touch the one repo it was made for.
- **Never paste it into chat, email, or a ticket.** All three are permanently searchable logs the moment the secret lands in them, long after anyone remembers it's still live there.
- **Prefer the peer's own hands on the keyboard.** The person who owns the Forgejo instance generates the token and writes it directly into `/etc/graft/<name>.token` themselves, over their own SSH session — it never transits through a third party (including you) at all.
- **When a third party must relay it, use something that dies after one read** — a password manager's one-time-share feature (Bitwarden Send, 1Password Psst), or a self-hosted equivalent (`onetimesecret.com` is open source and self-hostable). Never a plain link with no expiry.
- **Rotate it if you're ever unsure it stayed private** — regenerating a Forgejo token is free; guessing whether a six-month-old paste is still up is not.

See [Future: built-in token exchange](#future-built-in-token-exchange) at the bottom for ideas on `graft` doing this itself instead of leaning on outside tools.

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

## Adding a peer to an existing federation

Once `my-project` is mirrored between one Forgejo and one Radicle node, adding a second Forgejo instance to the same repo — a genuine federation, not just a pair — needs neither a new Radicle node nor any change to the two pairs already running.

| Placeholder | Stands for |
|---|---|
| `git2.example.org` | the second Forgejo instance |
| `bob` | the account on it that owns the mirrored repo |

**The one thing worth understanding first**: a Radicle repository (a RID) already replicates to every Radicle node that seeds it, automatically, regardless of `graft`. Pointing a second Forgejo pair's `radicle.http_base_url` at the *same* seed you're already using doesn't create a new relationship — that seed already has the content. Nothing to do on the Radicle side at all for this case; skip straight to the config.

1. On `git2.example.org`, create the repo (`auto_init: true`, same default branch) and a token, exactly as in step 2–3 above, scoped `write:repository` + `write:issue`, saved to its own file:

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

4. Confirm: the status dashboard now shows `git2.example.org` as a second row under `my-project`. If it joined an already-active federation, its side may show cells colored differently from a normal commit/issue/patch — "source" (this is where content first entered the federation) or "replicated" (this side holds it without `graft` ever having logged a push here itself, because Radicle's own gossip — or a Forgejo Authorized Integration — got there first). Hover one for the date, the original side, and a link.

## Adding a brand-new Radicle node to the mesh

Reusing an existing seed (above) needs nothing extra. Standing up a node nobody in the mesh has ever talked to is a two-step manual bootstrap — `graft`'s own Radicle node (`rad_home`) doesn't discover new peers on its own:

```sh
rad node connect <new-node-id>@<new-node-address>:8776
rad seed rad:z3gqcJUoA1n9HaHKufZs5FCSGazv5
```

Both matter. `rad node connect` is the first handshake — Radicle's peer discovery (`"peers": {"type": "dynamic"}` in `~/.radicle/config.json`) spreads pairs it already knows about, but a node nobody is yet connected to needs one explicit introduction. `rad seed` matters separately: a Radicle node's default seeding policy is to replicate *nothing* it isn't explicitly told to — being connected doesn't imply seeding. Skip either step and the new node stays either unreachable or silently empty.

## Troubleshooting

**"token does not have at least one of required scope(s)"** — the Forgejo token is missing `write:issue` or `write:repository`. Regenerate it with both.

**"forgejo and rad diverged since last sync"** — both sides got commits since the last pass that aren't ancestors of each other. `graft` won't guess which one wins; merge them by hand on either side and it'll pick up cleanly next pass.

**"'remote-rad' is not a git command"** — `git-remote-rad` (installed alongside `rad`) isn't on `PATH` for the user running `graft`. Add its directory to the systemd unit's `Environment=PATH=...`.

**A patch/PR never shows up on the other side** — check that the corresponding repo isn't still empty on the destination. The first git sync has to land before patches/issues referencing that content can be mirrored.

## Future: built-in token exchange

Today, getting a token from the peer who generated it into `graft`'s `token_file` is entirely a human problem — see [Sharing a token safely](#sharing-a-token-safely) above. `graft` could own more of that instead of leaning on outside tools. None of this is built; it's a menu of options worth weighing against each other before picking one, roughly cheapest-to-build first:

- **Point at an existing one-time-secret tool.** Zero code: document `onetimesecret.com` (self-hostable, open source) as the recommended relay when a direct SSH handoff isn't possible. Lowest effort, but `graft` doesn't control the experience or leave an audit trail of who claimed a token and when.

- **A `graft token share` command, minimal version.** `graft` generates a random opaque ID and a separate 6-digit passcode, stores the token encrypted (with a key derived from the passcode, so the encrypted-at-rest copy alone is useless), and serves it once at `https://graft.cyberwild.org/share/<id>` — the page asks for the passcode before it reveals anything, and the record is deleted server-side the moment it's successfully viewed (a second visit gets "already claimed", not the secret). A short TTL (15 minutes) covers the case where it's never claimed at all. Passcode communicated over a *different* channel than the link — a phone call, a second messaging app — so a leaked link alone is still useless. Straightforward to build on top of the state store already in place; the main design care is making sure the "delete on view" step is atomic (one claim, ever, even under a race).

- **QR code variant of the same page.** Same backend, but the sending admin displays a QR code on their own screen (in person, or screen-shared on a call) instead of sending a link at all — the receiving admin scans it with their phone. Puts the passcode in the URL fragment (`#passcode=...`) rather than a query param, so it decrypts client-side in the browser and never reaches `graft`'s own access logs. Nicer for a live handoff; no real advantage over the plain link for an async one.

- **A wormhole-style PAKE exchange** (the protocol behind [`magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)): both admins type the same short human-readable code (`7-crossover-clarinet`) into a small CLI, and a key exchange derives a shared secret from the code without either the code or the token ever crossing the wire in the clear — `graft`'s own server, even if fully compromised, never sees the plaintext token at any point. The strongest guarantee of the options here, and the most to build (needs a relay endpoint speaking the handshake, and both sides need shell access rather than just a browser) — worth it mainly if token handoffs become frequent enough to justify the investment.

- **Skip the handoff entirely.** The most elegant fix doesn't share a secret better — it avoids minting a long-lived one a human has to move at all. If Forgejo ever exposes a delegated token-creation flow (the peer's admin clicks a link, logs into *their own* Forgejo, and a token scoped to exactly `write:repository` + `write:issue` on exactly that repo is minted and handed to `graft` server-to-server), no plaintext secret ever needs to touch a clipboard, a chat window, or a QR code in the first place. Worth checking against Forgejo's OAuth2/application-token APIs before building any of the options above.
