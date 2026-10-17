#!/bin/sh
# The infrastructure scrub check: fails when a tracked file names the maintainers' private
# infrastructure (servers, hosts, home network, machine paths), which stays out of the public
# repos. The patterns are kept outside the repo, so the check never names them itself: one
# extended regular expression per line in $OMABOOT_SCRUB (default ~/.config/omaboot/scrub-patterns).
# Without that file there's nothing to check against, and it says so.
set -eu
cd "$(git rev-parse --show-toplevel)"
f=${OMABOOT_SCRUB:-$HOME/.config/omaboot/scrub-patterns}
if [ ! -r "$f" ]; then
	echo "scrub check skipped: no $f"
	exit 0
fi
if git grep -n -I -E -f "$f" -- "$@"; then
	echo "scrub check: the lines above name private infrastructure" >&2
	exit 1
fi
echo "scrub check: clean"
