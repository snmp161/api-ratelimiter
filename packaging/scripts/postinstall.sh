#!/bin/sh
set -e

# www-data is part of base-passwd on Debian/Ubuntu, but on minimal images
# (e.g. some containers) it can be missing. Create as a system user if so.
if ! getent passwd www-data >/dev/null; then
    adduser --system --no-create-home --group --quiet \
        --home /var/www --shell /usr/sbin/nologin \
        www-data
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    systemctl enable ratelimiter.service >/dev/null 2>&1 || true
    if [ "$1" = "configure" ] && systemctl is-active --quiet ratelimiter.service; then
        systemctl restart ratelimiter.service || true
    fi
fi

exit 0
