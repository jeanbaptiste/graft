# How a repo pair comes to exist

A "pair" is one entry in graft's sync loop: it links one Forgejo repository
to either a Radicle repository or a second Forgejo repository, and mirrors
whichever content types are enabled for it. There are **two completely
independent ways a pair gets created**, and graft itself won't show you
both at once unless you know to look:

## 1. Static — `config.yaml`

The pairs listed under `repos:` in the deployed `config.yaml`. This is
what a `git blame` / `cat /etc/graft/config.yaml` shows you. Adding one
requires editing the file and restarting the daemon.

## 2. Dynamic — the dashboard's self-service onboarding

The `/add-peer`, `/add-radicle-peer`, and `/new-repo` forms on the status
dashboard (`internal/admin`) let anyone who knows the admin password add a
pair **without touching config.yaml or restarting anything**. These rows
live in the `dynamic_repo` table inside the state database
(`/var/lib/graft/state.db` in production) and are invisible to anyone only
reading `config.yaml` — including a future session of an AI assistant, or a
human, planning a change from the config file alone.

**Before adding any new static pair, check for existing dynamic ones first:**

```bash
sqlite3 /var/lib/graft/state.db \
  "SELECT name, series, forgejo_base_url, forgejo_owner, forgejo_repo, radicle_rid FROM dynamic_repo WHERE approved = 1"
```

(or the Python equivalent if `sqlite3` isn't installed — `import sqlite3`
and the same query — both were used interchangeably while diagnosing the
incident this doc exists because of.)

## Why mixing the two on the *same* repo is dangerous

Confirmed live, not theoretical: a series ("constitution") already had a
working dynamic pair bridging `git.tricoteuses.fr` through the shared
Radicle RID. A static `forgejo_mirror` pair was then added, pairing the
*same two Forgejo repos* directly — without knowing the dynamic pair
existed, because nothing surfaces `dynamic_repo` rows next to
`config.yaml` at a glance.

The two pairs don't know about each other. Each one's `item_mapping`
table only tracks what *it* has mirrored. So each pair saw the *other's*
mirrored copy of a pre-existing issue as brand-new, unmirrored content —
and mirrored it again, which the other pair then also saw as new, and
mirrored again. **36 duplicate issues across two repos in under two
minutes**, only stopped by killing the daemon by hand.

## The guard that exists now

`cmd/sync/main.go`'s `checkPairCollisions` runs at startup, before any
pair is materialized: it builds one set of "claimed" repo targets
(`base_url + owner/repo`) from every static pair *and* every approved
dynamic pair, and refuses to start (loud, `os.Exit(1)`, not a silent skip)
if the same target is claimed twice. This turns "nobody remembered to
check" into "the daemon won't start until you resolve it" — it can't
prevent the collision from being *proposed*, but it stops it from ever
actually running.

If you hit this at startup: one of the two pairs it names needs to go —
either remove the static one from `config.yaml`, or find the dynamic one
in the dashboard's peer list and remove it there. Check which one is
already `materialized` and has real synced content before picking which
to keep.
