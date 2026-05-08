#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl stop ratelimiter.service >/dev/null 2>&1 || true
    systemctl disable ratelimiter.service >/dev/null 2>&1 || true
fi

exit 0
