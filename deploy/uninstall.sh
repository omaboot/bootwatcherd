#!/bin/bash
# Removes bootwatcherd from doiomad's server: its units, binary, settings and database. Nothing
# of doiomad's is touched, and the same checks as setup.sh's prove it. Run as root.
set -euo pipefail

DOIOMAD_UNITS="doiomad cloudflared litestream"
DOIOMAD_DB=/var/lib/doiomad/doesitomarchy.db
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

doiomad_state() {
	for u in $DOIOMAD_UNITS; do
		echo "$u $(systemctl show -p ActiveEnterTimestampMonotonic --value "$u") $(systemctl is-active "$u" || true)"
	done
	stat -c "db %U %G %a %i" "$DOIOMAD_DB"
}

before=$(doiomad_state)
echo "==> removing bootwatcherd"
systemctl disable -q --now bootwatcherd.service cloudflared-bootwatcherd.service 2>/dev/null || true
rm -f /etc/systemd/system/bootwatcherd.service /etc/systemd/system/cloudflared-bootwatcherd.service
systemctl daemon-reload
rm -f /usr/local/bin/bootwatcherd
rm -rf /etc/bootwatcherd /var/lib/private/bootwatcherd /var/lib/bootwatcherd
after=$(doiomad_state)
if [ "$before" != "$after" ]; then
	echo "!! doiomad changed during the removal:
before: $before
after:  $after" >&2
	exit 1
fi
curl -fsS -m 10 -o /dev/null http://127.0.0.1:8080/healthz || { echo "!! doiomad's /healthz doesn't answer" >&2; exit 1; }
echo "==> done: bootwatcherd gone, doiomad untouched"
