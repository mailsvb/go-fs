#!/bin/sh
# rpm passes 0 when the package is removed and 1 when it is upgraded. Only a
# removal stops and disables the service; an upgrade restarts it after.
#
# The configuration is a ghost of the package, which rpm deletes with it, so
# a removal keeps it as go-fs.toml.rpmsave first, as rpm does with a changed
# config file: it holds the accounts and the keys of whoever ran the server.
if [ "${1:-0}" -eq 0 ]; then
    if [ -d /run/systemd/system ]; then
        systemctl disable --now go-fs.service >/dev/null 2>&1 || :
    fi
    if [ -f /etc/go-fs/go-fs.toml ]; then
        mv -f /etc/go-fs/go-fs.toml /etc/go-fs/go-fs.toml.rpmsave || :
        echo "go-fs: the configuration is kept as /etc/go-fs/go-fs.toml.rpmsave"
    fi
fi
exit 0
