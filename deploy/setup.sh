#!/bin/bash
# Installs or updates bootwatcherd on doiomad's server. doiomad is the primary
# service there: this script touches nothing of doiomad's and proves it. It refuses to start
# unless doiomad is healthy, installs only bootwatcherd's own files, restarts only bootwatcherd's
# own units, then checks that no doiomad unit restarted and doiomad's database file is as it
# was; on any difference it stops bootwatcherd and fails.
#
# Run as root from a directory holding this script, import-sqlite.py, root/ and the bootwatcherd
# binary; deploy/provision.sh does it. /etc/bootwatcherd/pushover.env must exist (provision.sh
# writes it).
#
# Once, from v0.1.x (SQLite) to v0.2.0 (plain files): with bootwatcherd stopped for a few
# seconds, import-sqlite.py turns a copy of its database into the files. The database itself is
# left as it was, a backup the new version doesn't read.
set -euo pipefail
cd "$(dirname "$0")"

DOIOMAD_UNITS="doiomad cloudflared litestream"
DOIOMAD_DB=/var/lib/doiomad/doesitomarchy.db
MIN_AVAILABLE_MB=300
STATE=/var/lib/private/bootwatcherd # DynamicUser's StateDirectory

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
[ -x bootwatcherd ] || { echo "no bootwatcherd binary next to setup.sh" >&2; exit 1; }

step() { echo "==> $*"; }
fail() { echo "!! $*" >&2; exit 1; }

# doiomad's state: each unit's last start (monotonic, changes only on a restart) and state, and
# the database file's owner, group, mode and inode (metadata only: the file is never opened).
doiomad_state() {
	for u in $DOIOMAD_UNITS; do
		echo "$u $(systemctl show -p ActiveEnterTimestampMonotonic --value "$u") $(systemctl is-active "$u" || true)"
	done
	stat -c "db %U %G %a %i" "$DOIOMAD_DB"
}

doiomad_healthy() {
	for u in $DOIOMAD_UNITS; do
		systemctl is-active -q "$u" || { echo "$u isn't active"; return 1; }
	done
	curl -fsS -m 10 -o /dev/null http://127.0.0.1:8080/healthz || { echo "doiomad's /healthz doesn't answer"; return 1; }
}

# port_free PORT UNIT: nobody listens on PORT, or UNIT (ours, being updated) does.
port_free() {
	! ss -Hltn "sport = :$1" | grep -q . || systemctl is-active -q "$2"
}

step "preflight: doiomad first"
why=$(doiomad_healthy) || fail "not touching this server: $why"
avail=$(awk '/^MemAvailable:/ {print int($2 / 1024)}' /proc/meminfo)
[ "$avail" -ge $MIN_AVAILABLE_MB ] || fail "only ${avail} MB available (${MIN_AVAILABLE_MB} needed): not adding anything"
port_free 8471 bootwatcherd || fail "127.0.0.1:8471 is taken"
port_free 20251 cloudflared-bootwatcherd || fail "127.0.0.1:20251 is taken"
[ -s /etc/bootwatcherd/pushover.env ] || fail "/etc/bootwatcherd/pushover.env is missing (deploy/provision.sh writes it)"
migrate=no
if [ -f $STATE/bootwatcherd.db ] && [ ! -d $STATE/checks ]; then
	migrate=yes
	command -v python3 >/dev/null || fail "python3 is needed once, to move bootwatcherd's database into files"
fi
if [ -s /etc/bootwatcherd/tunnel.env ] || [ -s /etc/bootwatcherd/tunnel-token ]; then
	case "$(cloudflared tunnel run --help 2>&1 || true)" in
	*--token-file*) ;;
	*) fail "this cloudflared has no --token-file: update it first" ;;
	esac
fi
before=$(doiomad_state)
echo "    doiomad healthy, ${avail} MB available"

if [ $migrate = yes ]; then
	step "bootwatcherd's database into files (once)"
	systemctl stop bootwatcherd.service
	rm -rf /var/tmp/bootwatcherd-migrate
	install -d -m 0700 /var/tmp/bootwatcherd-migrate
	cp -p $STATE/bootwatcherd.db /var/tmp/bootwatcherd-migrate/
	[ ! -f $STATE/bootwatcherd.db-wal ] || cp -p $STATE/bootwatcherd.db-wal /var/tmp/bootwatcherd-migrate/
	if ! python3 -I import-sqlite.py /var/tmp/bootwatcherd-migrate/bootwatcherd.db /var/tmp/bootwatcherd-migrate/out; then
		rm -rf /var/tmp/bootwatcherd-migrate
		systemctl start bootwatcherd.service
		fail "the move failed: bootwatcherd is back as it was"
	fi
	mv /var/tmp/bootwatcherd-migrate/out/checks /var/tmp/bootwatcherd-migrate/out/state.json $STATE/
	[ ! -f /var/tmp/bootwatcherd-migrate/out/days.tsv ] || mv /var/tmp/bootwatcherd-migrate/out/days.tsv $STATE/
	chown -R --reference=$STATE $STATE/checks $STATE/state.json
	[ ! -f $STATE/days.tsv ] || chown --reference=$STATE $STATE/days.tsv
	rm -rf /var/tmp/bootwatcherd-migrate
	echo "    bootwatcherd.db stays as it was: a backup the new version doesn't read"
fi

step "files (bootwatcherd's own only)"
install -d -o root -g root -m 0750 /etc/bootwatcherd
install -m 0755 bootwatcherd /usr/local/bin/bootwatcherd.new
mv -f /usr/local/bin/bootwatcherd.new /usr/local/bin/bootwatcherd
# the tunnel's token as a file for --token-file (tunnel.env stays for the previous unit)
if [ ! -s /etc/bootwatcherd/tunnel-token ] && [ -s /etc/bootwatcherd/tunnel.env ]; then
	sed -n 's/^TUNNEL_TOKEN=//p' /etc/bootwatcherd/tunnel.env | tr -d '\n' | install -o root -g root -m 0600 /dev/stdin /etc/bootwatcherd/tunnel-token
fi
[ ! -f /etc/systemd/system/cloudflared-bootwatcherd.service ] ||
	install -m 0644 /etc/systemd/system/cloudflared-bootwatcherd.service /etc/bootwatcherd/cloudflared-bootwatcherd.service.previous
for f in bootwatcherd.service cloudflared-bootwatcherd.service; do
	install -m 0644 "root/etc/systemd/system/$f" "/etc/systemd/system/$f"
done
systemctl daemon-reload # reads unit files; restarts nothing

step "bootwatcherd's units"
systemctl enable -q bootwatcherd.service
systemctl restart bootwatcherd.service
if [ -s /etc/bootwatcherd/tunnel-token ]; then
	systemctl enable -q cloudflared-bootwatcherd.service
	systemctl restart cloudflared-bootwatcherd.service
else
	echo "    cloudflared-bootwatcherd: waiting for /etc/bootwatcherd/tunnel-token (deploy/cloudflare.sh)"
fi

step "after: bootwatcherd up, doiomad untouched"
sleep 3
rollback() {
	systemctl disable -q --now bootwatcherd.service cloudflared-bootwatcherd.service || true
	fail "$1: bootwatcherd stopped"
}
systemctl is-active -q bootwatcherd.service || rollback "bootwatcherd didn't start"
curl -fsS -m 5 -o /dev/null http://127.0.0.1:8471/healthz || rollback "bootwatcherd doesn't answer on 127.0.0.1:8471"
# the tunnel up with the new unit (its metrics' /ready), or the previous unit back
tunnel_ready() {
	for _ in $(seq 20); do
		curl -fsS -m 2 -o /dev/null http://127.0.0.1:20251/ready && return 0
		sleep 1
	done
	return 1
}
if [ -s /etc/bootwatcherd/tunnel-token ] && ! tunnel_ready; then
	if [ -f /etc/bootwatcherd/cloudflared-bootwatcherd.service.previous ]; then
		install -m 0644 /etc/bootwatcherd/cloudflared-bootwatcherd.service.previous /etc/systemd/system/cloudflared-bootwatcherd.service
		systemctl daemon-reload
		systemctl restart cloudflared-bootwatcherd.service
		fail "cloudflared-bootwatcherd wasn't ready with the new unit: the previous unit is back"
	fi
	fail "cloudflared-bootwatcherd isn't ready"
fi
why=$(doiomad_healthy) || rollback "doiomad isn't healthy after the install: $why"
after=$(doiomad_state)
[ "$before" = "$after" ] || rollback "doiomad changed during the install:
before: $before
after:  $after"
echo "    no doiomad unit restarted; doiomad's database file unchanged"
for u in bootwatcherd cloudflared-bootwatcherd; do
	printf '    %s: %s, %s MB, user %s\n' "$u" "$(systemctl is-active "$u" || true)" \
		"$(( $(systemctl show -p MemoryCurrent --value "$u" | sed 's/\[not set\]/0/') / 1048576 ))" \
		"$(ps -o user=,uid= -p "$(systemctl show -p MainPID --value "$u")" | xargs)"
done
echo "    state directory: $(stat -c '%U:%G (%u:%g) %a' $STATE)"

step "test push (owner, 2026-10-17: every deploy proves pushes arrive)"
systemd-run --quiet --wait --pipe --collect -p DynamicUser=yes -p NoNewPrivileges=yes \
	-p LoadCredential=pushover.env:/etc/bootwatcherd/pushover.env /usr/local/bin/bootwatcherd -test-push ||
	fail "deployed, but the test push didn't go out"
step "done"
