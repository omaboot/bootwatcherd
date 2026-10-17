#!/usr/bin/env python3
"""Moves bootwatcherd's SQLite database (v0.1.x) into its plain files (v0.2.0 on), once.

    python3 -I import-sqlite.py COPY_OF_bootwatcherd.db OUTDIR

Reads a copy of the database (setup.sh copies it with the service stopped; the original is
never opened) and writes OUTDIR/checks/<day>.tsv, OUTDIR/days.tsv (summaries of days with no
checks left) and OUTDIR/state.json, as store.go reads them. Standard library only.

Echo failures that were the checker's own stale connection ("connection reset by peer" on the
connection kept from the previous check, which v0.2.0 no longer keeps) become no data, with
the reason saying so (owner, 2026-10-17). Everything else moves as it was.
"""

import json
import os
import sqlite3
import sys
from datetime import datetime, timezone

ENDPOINTS = ["/v1/echo", "/v1/stream", "/v1/sink"]
MAX_ERR = 200  # store.go's maxErr
STALE = "connection reset by peer"


def clip(s, n=MAX_ERR):
    """probe.go's clip: one line, at most n bytes, cut on a character boundary with an ellipsis."""
    s = s.replace("\t", " ").replace("\n", " ").replace("\r", " ")
    b = s.encode()
    if len(b) <= n:
        return s
    return b[: n - len("…".encode())].decode(errors="ignore") + "…"


def day_of(unix):
    return datetime.fromtimestamp(unix, timezone.utc).strftime("%Y-%m-%d")


def rfc3339(unix):
    return datetime.fromtimestamp(unix, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def main(db_path, out):
    if os.path.exists(os.path.join(out, "checks")):
        sys.exit(f"{out}/checks exists already: nothing done")
    db = sqlite3.connect(db_path)
    order = {p: i for i, p in enumerate(ENDPOINTS)}
    rows = [r for r in db.execute("SELECT at, endpoint, state, rtt_ms, err FROM checks") if r[1] in order]
    rows.sort(key=lambda r: (r[0], order[r[1]]))

    lines, stale = {}, 0
    for at, path, state, rtt, err in rows:
        if path == "/v1/echo" and state == 2 and STALE in err:
            state, rtt, err = 0, 0, "the checker's stale connection, not boot.watch: " + err
            stale += 1
        lines.setdefault(day_of(at), []).append(f"{at}\t{path}\t{state}\t{rtt}\t\t{clip(err)}\n")

    os.makedirs(os.path.join(out, "checks"), mode=0o700)
    for day, ls in lines.items():
        with open(os.path.join(out, "checks", day + ".tsv"), "w", opener=private) as f:
            f.writelines(ls)

    old = [r for r in db.execute("SELECT day, endpoint, good, failed, median_ms FROM days ORDER BY day, endpoint")
           if r[0] not in lines and r[1] in order]
    if old:
        with open(os.path.join(out, "days.tsv"), "w", opener=private) as f:
            f.writelines(f"{d}\t{p}\t{g}\t{fl}\t{m}\t\n" for d, p, g, fl, m in old)

    alerts = {}
    for path, fails, since, pushed, last_err in db.execute("SELECT endpoint, fails, since, pushed, last_err FROM alerts"):
        a = {"fails": fails, "pushed": bool(pushed)}
        if since:
            a["since"] = rfc3339(since)
        if last_err:
            a["last_err"] = last_err
        alerts[path] = a
    with open(os.path.join(out, "state.json"), "w", opener=private) as f:
        json.dump({"alerts": alerts, "blind": {"fails": 0, "pushed": False}}, f, indent="\t")
        f.write("\n")

    if not rows:
        print(f"    no checks; {len(old)} older day summaries, {len(alerts)} alert states")
        return
    written = sum(len(ls) for ls in lines.values())
    if written != len(rows):
        sys.exit(f"wrote {written} lines for {len(rows)} checks")
    print(f"    {len(rows)} check rows over {len(lines)} days ({min(lines)} to {max(lines)}), "
          f"{stale} stale-connection echo failures now no data, {len(old)} older day summaries, "
          f"{len(alerts)} alert states")


def private(path, flags):
    return os.open(path, flags, 0o600)


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
