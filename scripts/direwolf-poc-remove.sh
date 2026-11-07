#!/bin/bash
# Undo direwolf-poc-install.sh. Leaves the direwolf package installed
# (apt-get remove direwolf to drop it) and the airtime trace in /var/lib/private/airtime-hri200.
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
systemctl disable --now airtime-hri200.service direwolf-hri200.service hri200-direwolf-feed.service hri200-txinh.service 2>/dev/null || true
rm -f /etc/systemd/system/airtime-hri200.service /etc/systemd/system/direwolf-hri200.service \
      /etc/systemd/system/hri200-direwolf-feed.service /etc/systemd/system/hri200-txinh.service \
      /etc/direwolf-hri200.conf /usr/local/bin/hri200-analog-feed /usr/local/bin/airtime \
      /usr/local/sbin/hri200-txinh-setup
systemctl daemon-reload
systemctl is-active hri200-analog hri200-ysf-proxy
