#!/bin/sh
# The entrypoint of the container image. go-fs writes the starter
# configuration into /etc/go-fs itself when there is none yet, in a named
# volume and in a folder of the host alike. It runs as process 1, with the
# arguments given to the container after its own, so that docker stop
# reaches it.
#
# A named volume takes the owner of the image's folder, UID 10001, so a
# container started with --user as another UID cannot write into it. That is
# said here, with the command that hands the volume over, rather than left to
# a bare "Permission denied".
set -eu

folder=/etc/go-fs
config=$folder/go-fs.toml
uid=$(id -u)
gid=$(id -g)

# explain PATH MOUNT: why PATH cannot be used by the UID go-fs runs as, and
# how to hand the volume at MOUNT to that UID. Docker fills an empty named
# volume from the image at every start, the owner of its folder included, so
# the command leaves a file in it for the chown to last.
explain() {
	echo "go-fs runs as $uid:$gid, but $1 belongs to $(stat -c '%u:%g, mode %a' "$1")." >&2
	echo "Hand the volume or folder mounted at $2 to $uid:$gid once, for a named volume with" >&2
	echo "  docker run --rm --user 0 -v <volume>:$2 --entrypoint sh <image> -c 'touch $2/.keep && chown -R $uid:$gid $2'" >&2
	echo "or start the container without --user, as UID 10001." >&2
}

if [ ! -w "$folder" ] || [ ! -x "$folder" ]; then
	echo "error: cannot write into $folder" >&2
	explain "$folder" "$folder"
	exit 1
fi
if [ -e "$config" ] && { [ ! -r "$config" ] || [ ! -w "$config" ]; }; then
	echo "error: cannot read and write $config" >&2
	explain "$config" "$folder"
	exit 1
fi

# The folders served by default only warn: a configuration may serve others.
for dir in /var/lib/go-fs /var/lib/go-fs/*/; do
	dir=${dir%/}
	[ -d "$dir" ] || continue
	if [ ! -w "$dir" ] || [ ! -x "$dir" ]; then
		echo "warning: cannot write into $dir" >&2
		explain "$dir" /var/lib/go-fs
	fi
done

exec /usr/bin/go-fs -config "$config" "$@"
