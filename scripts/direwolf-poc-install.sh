#!/bin/bash
# Install the receive-only direwolf PoC on the HRI-200 host. Run with sudo from
# the directory holding: hri200-analog-feed airtime direwolf-hri200.conf
# hri200-direwolf-feed.service direwolf-hri200.service airtime-hri200.service
# hri200-txinh-setup hri200-txinh.service
# Adds only new files and units; existing hri200-* services are not touched.
# Undo: direwolf-poc-remove.sh
set -euo pipefail
cd "$(dirname "$0")"
[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }

if ! command -v direwolf >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends direwolf
fi
# The distro package must not run a direwolf of its own on the default sound card.
for u in direwolf.service; do
    if systemctl list-unit-files "$u" >/dev/null 2>&1 && systemctl is-enabled -q "$u" 2>/dev/null; then
        systemctl disable --now "$u"
    fi
done

install -m 0755 hri200-analog-feed /usr/local/bin/hri200-analog-feed
install -m 0755 airtime /usr/local/bin/airtime
install -m 0755 hri200-txinh-setup /usr/local/sbin/hri200-txinh-setup
install -m 0644 direwolf-hri200.conf /etc/direwolf-hri200.conf
install -m 0644 hri200-direwolf-feed.service direwolf-hri200.service airtime-hri200.service hri200-txinh.service /etc/systemd/system/
systemctl daemon-reload
# TXINH's line must exist before direwolf-hri200 starts (it Requires it).
systemctl enable --now hri200-txinh.service hri200-direwolf-feed.service direwolf-hri200.service airtime-hri200.service
sleep 3
systemctl is-active hri200-txinh hri200-direwolf-feed direwolf-hri200 airtime-hri200 hri200-analog hri200-ysf-proxy
