#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

# Do NOT remove www-data on purge — it is shared with nginx/apache/php-fpm
# and likely belongs to other packages.

exit 0
