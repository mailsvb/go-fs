#!/bin/sh
# rpm passes 0 when the package is removed and 1 when it is upgraded. Only a
# removal stops and disables the service; an upgrade restarts it after. The
# configuration in /var/lib/go-fs is not the package's, so it is kept either
# way: it holds the accounts and the keys of whoever ran the server.
if [ "${1:-0}" -eq 0 ] && [ -d /run/systemd/system ]; then
    systemctl disable --now go-fs.service >/dev/null 2>&1 || :
fi
exit 0
