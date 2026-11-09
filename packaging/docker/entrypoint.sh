#!/bin/sh
# The entrypoint of the container image. A named volume on /etc/go-fs is
# filled from the image, but a folder of the host mounted there is not, so
# the configuration the image ships is put in place when there is none yet.
# Then go-fs runs as process 1, with the arguments given to the container
# after its own, so that docker stop reaches it.
set -eu

config=/etc/go-fs/go-fs.toml
if [ ! -e "$config" ]; then
	cp /usr/share/go-fs/go-fs.toml "$config"
	chmod 0600 "$config"
	echo "wrote the starter configuration to $config"
fi

exec /usr/bin/go-fs -config "$config" "$@"
