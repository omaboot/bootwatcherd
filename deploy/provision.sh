#!/bin/bash
# Builds bootwatcherd and installs it on doiomad's server, where it is a guest:
# deploy/setup.sh refuses to run unless doiomad is healthy and proves afterwards that nothing of
# doiomad's changed. Re-running this deploys a new build. It never creates or resizes a droplet.
#
# Environment:
#   PUSHOVER_TOKEN, PUSHOVER_USER   written to /etc/bootwatcherd/pushover.env (first run; later
#                                   runs keep the file when they're unset)
#   DROPLET      doiomad's server; DO_CONTEXT its doctl profile
#   ADMIN_KEY    the admin SSH key; ADMIN_USER admin login (default: $USER)
# The names can live in deploy/local.env instead (untracked; copy deploy/local.env.example).
set -euo pipefail
cd "$(dirname "$0")"
[ ! -f local.env ] || . ./local.env

NAME=${DROPLET:?set DROPLET in deploy/local.env}
ADMIN_USER=${ADMIN_USER:-$USER}
ADMIN_KEY=${ADMIN_KEY:?set ADMIN_KEY in deploy/local.env}
step() { echo "==> $*"; }

ip=$(doctl --context "${DO_CONTEXT:?set DO_CONTEXT in deploy/local.env}" compute droplet list --format Name,PublicIPv4 --no-header | awk -v n="$NAME" '$1==n {print $2}')
[ -n "$ip" ] || { echo "droplet $NAME not found" >&2; exit 1; }
ssh_opts=(-o ConnectTimeout=10 -o IdentitiesOnly=yes -i "$ADMIN_KEY")

# Production runs releases: a clean tree at a tag (vX.Y.Z), so /status.json names exactly what runs.
if [ "${FORCE:-}" != 1 ]; then
	[ -z "$(git -C .. status --porcelain)" ] || { echo "uncommitted changes: commit and tag a release first (or FORCE=1)" >&2; exit 1; }
	git -C .. describe --tags --exact-match HEAD >/dev/null 2>&1 || { echo "HEAD isn't a release tag: git tag vX.Y.Z && git push --tags (or FORCE=1)" >&2; exit 1; }
fi
step "building bootwatcherd $(git -C .. describe --tags --always --dirty)"
(cd .. && make build >/dev/null)

if [ -n "${PUSHOVER_TOKEN:-}" ]; then
	step "writing the Pushover credentials"
	printf 'PUSHOVER_TOKEN=%s\nPUSHOVER_USER=%s\n' "$PUSHOVER_TOKEN" "${PUSHOVER_USER:?}" |
		ssh "${ssh_opts[@]}" "$ADMIN_USER@$ip" \
			"sudo install -d -o root -g root -m 0750 /etc/bootwatcherd && sudo install -o root -g root -m 0600 /dev/stdin /etc/bootwatcherd/pushover.env"
fi

step "running setup.sh on $NAME ($ip)"
# a fixed path is safe in /tmp here: rm can't remove another user's directory there (sticky
# bit), and mkdir without -p fails if anyone made it in between
tar -czf - setup.sh import-sqlite.py uninstall.sh root -C ../dist bootwatcherd | ssh "${ssh_opts[@]}" "$ADMIN_USER@$ip" \
	"rm -rf /tmp/bootwatcherd-setup && mkdir -m 0700 /tmp/bootwatcherd-setup && tar -xzf - -C /tmp/bootwatcherd-setup &&
	 sudo /tmp/bootwatcherd-setup/setup.sh; rc=\$?; rm -rf /tmp/bootwatcherd-setup; exit \$rc"
