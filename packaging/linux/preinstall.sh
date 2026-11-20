#!/bin/sh
# The account the service runs as, made before the files are put in place,
# since they are owned by it. rpm passes 1 on an install and 2 on an upgrade;
# either way an account that is there already is kept as it is.
getent group go-fs >/dev/null || groupadd --system go-fs
getent passwd go-fs >/dev/null || useradd --system --gid go-fs \
    --home-dir /var/lib/go-fs --no-create-home --shell /sbin/nologin \
    --comment "go-fs file server" go-fs

# Packages up to 0.22 kept the configuration in /etc/go-fs, and the
# persistent data file beside it; both move to /var/lib/go-fs, where
# everything else is. This runs before the files of the old package are
# removed, since that package owns /etc/go-fs/go-fs.toml as a ghost, which
# rpm deletes with it. mv keeps the owner and the mode. Anything else in
# /etc/go-fs, a key a configuration names for instance, stays where it is,
# and so does the folder then.
old=/etc/go-fs
new=/var/lib/go-fs
if [ -f "$old/go-fs.toml" ] && [ ! -e "$new/go-fs.toml" ]; then
    install -d -o go-fs -g go-fs -m 0750 "$new"
    for file in go-fs.toml go-fs.db; do
        [ -e "$old/$file" ] && mv -f "$old/$file" "$new/$file"
    done
    echo "go-fs: moved the configuration from $old to $new/go-fs.toml"
fi
exit 0
