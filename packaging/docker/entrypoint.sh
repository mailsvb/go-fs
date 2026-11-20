#!/bin/sh
# The entrypoint of the container image. Everything go-fs keeps is in
# /var/lib/go-fs, the configuration included: go-fs writes the starter
# configuration there itself when there is none yet, in a named volume and
# in a folder of the host alike. It runs as process 1, with the arguments
# given to the container after its own, so that docker stop reaches it.
#
# A named volume takes the owner of the image's folder, UID 1000, so a
# container started with --user as another UID cannot write into it. That is
# said here, with the command that hands the volume over, rather than left to
# a bare "Permission denied".
set -eu

folder=/var/lib/go-fs
config=$folder/go-fs.toml
uid=$(id -u)
gid=$(id -g)

# explain PATH: why PATH cannot be used by the UID go-fs runs as, and how to
# hand the volume to that UID. Docker fills an empty named volume from the
# image at every start, the owner of its folder included, so the command
# leaves a file in it for the chown to last.
explain() {
	echo "go-fs runs as $uid:$gid, but $1 belongs to $(stat -c '%u:%g, mode %a' "$1")." >&2
	echo "Hand the volume or folder mounted at $folder to $uid:$gid once, for a named volume with" >&2
	echo "  docker run --rm --user 0 -v <volume>:$folder --entrypoint sh <image> -c 'touch $folder/.keep && chown -R $uid:$gid $folder'" >&2
	echo "or start the container without --user, as UID 1000." >&2
}

if [ ! -w "$folder" ] || [ ! -x "$folder" ]; then
	echo "error: cannot write into $folder" >&2
	explain "$folder"
	exit 1
fi
if [ -e "$config" ] && { [ ! -r "$config" ] || [ ! -w "$config" ]; }; then
	echo "error: cannot read and write $config" >&2
	explain "$config"
	exit 1
fi

# The folders served by default only warn: a configuration may serve others.
for dir in "$folder"/*/; do
	dir=${dir%/}
	[ -d "$dir" ] || continue
	if [ ! -w "$dir" ] || [ ! -x "$dir" ]; then
		echo "warning: cannot write into $dir" >&2
		explain "$dir"
	fi
done

exec /usr/bin/go-fs -config "$config" "$@"
