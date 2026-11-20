# The container image of go-fs, for linux/amd64, built by make docker from
# build/pkg/docker, which make docker-inputs fills: the binary built with
# -X main.packaging=docker, which writes the image's starter configuration
# to /var/lib/go-fs/go-fs.toml at the first start, the entrypoint and the
# licence. Everything go-fs keeps is in /var/lib/go-fs, its one volume. The
# release workflow pushes it to ghcr.io/mailsvb/go-fs.
#
# Besides go-fs it carries only what Fetch runs: yt-dlp, the ffmpeg and
# ffprobe it merges and converts with, and deno, which yt-dlp solves the
# JavaScript challenges of YouTube with. go-fs finds them on the PATH itself,
# so the configuration names none of them.
FROM alpine:3.24

# The standalone yt-dlp for musl bundles its own Python and the scripts it runs
# under deno, so the image needs no python3. YouTube breaks an old yt-dlp
# quickly, so a release passes the latest version; this default is for a
# local build.
ARG YTDLP_VERSION=2026.10.26
ARG VERSION=dev

LABEL org.opencontainers.image.title="go-fs" \
      org.opencontainers.image.description="FTP, FTPS, SFTP, HTTP, HTTPS, an S3 compatible API, TFTP and NBD in a single binary" \
      org.opencontainers.image.source="https://github.com/mailsvb/go-fs" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      io.github.mailsvb.go-fs.yt-dlp.version="${YTDLP_VERSION}"

# ffmpeg brings ffprobe; tzdata lets TZ set the time zone of the log
RUN apk add --no-cache ca-certificates tzdata ffmpeg deno \
 && addgroup -S -g 1000 go-fs \
 && adduser -S -D -H -u 1000 -G go-fs -h /var/lib/go-fs -s /sbin/nologin go-fs

# the download is checked against the checksums of the same release
RUN set -eu; \
    base="https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}"; \
    cd /tmp; \
    wget -q "$base/yt-dlp_musllinux" "$base/SHA2-256SUMS"; \
    grep ' yt-dlp_musllinux$' SHA2-256SUMS | sha256sum -c -; \
    install -m 0755 yt-dlp_musllinux /usr/local/bin/yt-dlp; \
    rm -f yt-dlp_musllinux SHA2-256SUMS

COPY go-fs /usr/bin/go-fs
COPY entrypoint.sh /usr/bin/go-fs-entrypoint
COPY LICENSE /usr/share/licenses/go-fs/LICENSE

# The modes are set here rather than by COPY --chmod, which the classic
# builder lacks. /var/lib/go-fs holds the configuration and the folders it
# serves from, all owned by the account go-fs runs as, 1000:1000. go-fs
# writes into its configuration file by renaming a new one over it, so the
# whole folder is the volume, not the file. A named volume is filled from
# these folders, owner and modes included, so it belongs to 1000 too: a
# container started with --user as another UID needs the volume handed to
# that UID once, or a folder of the host that UID owns. The entrypoint says
# so and prints the command.
RUN chmod 0755 /usr/bin/go-fs /usr/bin/go-fs-entrypoint \
 && chmod 0644 /usr/share/licenses/go-fs/LICENSE \
 && install -d -o go-fs -g go-fs -m 0750 /var/lib/go-fs \
 && for dir in files registry disks; do install -d -o go-fs -g go-fs -m 0750 /var/lib/go-fs/$dir; done \
 && for dir in temp trash; do install -d -o go-fs -g go-fs -m 0700 /var/lib/go-fs/$dir; done

# each tool once, so that an image whose tools do not start is not built
RUN go-fs -version && ffmpeg -version >/dev/null && ffprobe -version >/dev/null \
 && deno --version && yt-dlp --version

USER go-fs
VOLUME ["/var/lib/go-fs"]
# http, https, ftp, ftps, sftp, tftp and nbd at the ports of the configuration;
# only http is switched on
EXPOSE 8080 8443 8021 8990 8022 8069/udp 10809
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/bin/go-fs-entrypoint"]
