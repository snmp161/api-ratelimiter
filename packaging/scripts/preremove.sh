#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl stop api-ratelimiter.service >/dev/null 2>&1 || true
    systemctl disable api-ratelimiter.service >/dev/null 2>&1 || true
fi

exit 0
