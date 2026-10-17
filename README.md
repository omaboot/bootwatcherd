# bootwatcherd

boot.watch's monitor and status page, https://status.boot.watch. Every minute it checks
bootwatchd the way an OmaBoot? Live Mac does, through boot.watch's public address (Cloudflare,
the tunnel, bootwatchd), keeps 60 days of results in its own plain files, pushes an alert
through Pushover when an endpoint stays down or it can't check, and serves the status page (the
W1 design). Standard library only.

## A check

At second 30 of every minute, with a 20 s cap, on a connection of its own: each check opens a
fresh HTTP/1.1 connection and closes it at the end, as OmaBoot? Live's client does for each test
(boot-live's `internal/bootwatch`), with the default dialing (IPv6 first where there is one; each
result records the family). A connection kept from one check to the next went stale behind
Cloudflare and failed the next check's echo, and a slow path stayed with it until it was replaced.

1. **Control:** `GET https://www.cloudflare.com/cdn-cgi/trace`. If it fails, the checker's own
   network is the problem: every endpoint is *no data* for that minute, and nothing pages.
2. **Echo:** a random nonce to `/v1/echo`; good when it comes back exactly.
3. **Stream:** 16 KB from `/v1/stream` with a fresh seed; good when every byte matches the
   AES-128-CTR stream (bootwatcherd's own copy, pinned to bootwatchd's README test vectors).
4. **Sink:** 16 KiB of another seed's stream to `/v1/sink`; good when bootwatchd says all 16,384
   matched.
5. **Counters:** `/v1/health`, for the page's daily-budget line, and bootwatchd's start time: a
   new one is a restart, which shows even when it fell between two checks.

About 47 MB a day, ~1% of bootwatchd's 4 GiB daily budget. A failure's reason is kept to one
line of at most 200 bytes.

## The page

A day's bar (UTC days, 60 of them): teal when every check was good and the 95th percentile round
trip at most 300 ms (one check in 20 may be slower); amber when some failed (under 5%) or more
were slower; red when 5% or more failed; grey with no checks. A bar's tooltip has the day's
counts, median and 95th percentile, and bootwatchd's restarts that day. Uptime is good checks over
good and failed checks across the 60 days; no-data minutes count for neither. The headline goes
"Status unknown" when the last check is more than 3 minutes old. bootwatchd keeps the day's byte
count across restarts; when its count started after midnight (`day_bytes_since`: its first start
that day), the budget line says since when.

Server-rendered once per check (and every 30 s), so visitors cost the checker next to nothing; no
JavaScript, reloads every 60 s, self-hosted JetBrains Mono (OFL), a Content-Security-Policy that
allows only the page's own stylesheet (by its hash) and fonts, HSTS, plain HTTP sent to HTTPS,
and no request log. Why a check failed never reaches the page (Go's network errors name the
server's own address). `/status.json` has the same data.

`/fresh` is for a monitor elsewhere: 200 while the checks keep coming, 503 when the last is more
than 3 minutes old. Point one at `https://status.boot.watch/fresh` (an HTTP monitor that pages on
anything but 200) and it covers what bootwatcherd can't report itself: bootwatcherd stuck, its
server down, the tunnel down.

## Alerts

One Pushover push per state change: when endpoints fail 3 checks in a row (no-data minutes
neither count nor break the run), and when they're good again. Endpoints that change in the same
check share one push ("boot.watch not answering" when all three go down together); an endpoint
that changes at another check gets its own. An undelivered push is retried on the next failed
check. The state lives in `state.json`, so a restart doesn't repeat a push.

When the control check fails 10 times in a row, the checker can't check: one push says so, and
one when it can again. If the network itself was down and nothing could go out, one push
afterwards says when it was blind. `-test-push` sends one push and exits; every deploy runs it.

## Running

```
make build
./dist/bootwatcherd -listen 127.0.0.1:8471 -data bootwatcherd-data -pushover pushover.env
```

| Flag | |
|---|---|
| `-listen` | Where the page listens (default `127.0.0.1:8471`). |
| `-data` | The directory for its files, created if missing (default `$STATE_DIRECTORY`, else `bootwatcherd-data`). |
| `-target` | The bootwatchd to watch (default `https://boot.watch`). |
| `-control` | The control URL (default Cloudflare's `/cdn-cgi/trace`). |
| `-pushover` | `PUSHOVER_TOKEN=` and `PUSHOVER_USER=` lines (default `$CREDENTIALS_DIRECTORY/pushover.env`); without it, no pushes. |
| `-page` | The page's address, linked from pushes; plain-HTTP visits go to it. |
| `-test-push` | Send one push to show pushes arrive, then exit. |

## The files

All plain text, in the state directory; the last 60 days are in memory too (about 1 MB).

| File | |
|---|---|
| `checks/<UTC day>.tsv` | One line per endpoint per check: unix time, endpoint, state (0 no data, 1 good, 2 failed), round trip in ms, address family, why it failed. One write per check; a line a crash cut short is skipped. Deleted after 60 days. |
| `days.tsv` | The summaries of deleted days: date, endpoint, good, failed, median and 95th percentile ms. |
| `state.json` | The alert states and bootwatchd's restarts, rewritten whole (a new file renamed over the old) when they change. |

Up to v0.1.x it kept an SQLite database (`bootwatcherd.db`); the first v0.2.0 deploy moves it
into the files with `deploy/import-sqlite.py` (Python's standard library, run once by `setup.sh`
with bootwatcherd stopped for a few seconds) and leaves the database as it was, a backup the new
version doesn't read. The move counts the old echo failures that were the checker's own stale
connection ("connection reset by peer" on the kept connection) as no data, saying so in the
reason.

## Deploying: a guest on doiomad's server

bootwatcherd runs on the DoesItOmarchy.com server, behind its own Cloudflare Tunnel.
doiomad is the primary and more important service there, so bootwatcherd must never disturb it:

- **Its own everything:** its own database (`/var/lib/bootwatcherd/bootwatcherd.db`), its own
  units (`bootwatcherd`, `cloudflared-bootwatcherd`), its own tunnel and token, its own settings
  (`/etc/bootwatcherd`). doiomad's database, settings and program directories, and Litestream's
  config, are invisible to both units (`InaccessiblePaths`); Litestream keeps replicating
  doiomad's database alone.
- **Small and yielding:** memory caps of 64 MB and 96 MB, CPU and I/O weight 20 (doiomad's are
  100), a 20% CPU ceiling, `Nice=10`, `OOMScoreAdjust=500` (killed before doiomad), logs on
  state changes only. Its cloudflared's metrics are on 127.0.0.1:20251, away from doiomad's.
- **Proved at every install:** `deploy/setup.sh` refuses to start unless doiomad, its
  cloudflared and Litestream are active, doiomad's `/healthz` answers and 300 MB are free; it
  installs only bootwatcherd's files and restarts only its units; then it checks that no doiomad
  unit restarted and that doiomad's database file (owner, mode, inode; metadata only) is as it
  was, and stops bootwatcherd if anything differs. Its cloudflared must answer on `/ready` with
  the new unit, or the previous unit goes back. Last, it sends a test push.
  `deploy/uninstall.sh` removes everything with the same checks.

Steps, from a laptop with `doctl` and the admin SSH key; the server's name, the doctl profile and
the key go in `deploy/local.env` (untracked: copy `deploy/local.env.example`):

1. `PUSHOVER_TOKEN=… PUSHOVER_USER=… deploy/provision.sh`: builds, writes
   `/etc/bootwatcherd/pushover.env` (when the variables are set), runs `setup.sh`. Re-running it
   deploys a new build. The Pushover application token and user key are in the maintainers'
   password manager.
2. `CF_API_TOKEN=… deploy/cloudflare.sh`: the `bootwatcherd` tunnel to 127.0.0.1:8471 for
   `status.boot.watch`, its proxied DNS record, and the tunnel token on the server
   (`/etc/bootwatcherd/tunnel-token`, a credential cloudflared reads with `--token-file`). The
   token is in the maintainers' password manager. boot.watch's redirect rule exempts
   `status.boot.watch`; bootwatchd's `deploy/cloudflare.sh` owns that rule.

## Releases

Production runs tagged releases. Commit, tag (`git tag v0.2.0 && git push --tags`), then run
`deploy/provision.sh`: it refuses uncommitted changes or a HEAD that isn't a tag (`FORCE=1`
overrides, for an emergency). The build embeds `git describe --tags --always --dirty`;
`/status.json` reports it as `version`, and the startup log names it. Tags only: no GitHub
Releases or attached binaries.

## Development

`make check` runs vet, the tests and gofmt.
