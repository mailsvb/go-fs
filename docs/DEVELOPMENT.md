# Development

Building go-fs from source, how versions are stamped, and how a release is made.

## Build

Requires Go 1.27 or newer.

```shell
make build          # the same binaries as a release, stamped as a dev build
make test           # the test suite
make race           # the test suite under the race detector
make release        # the signed .update files of a release into dist/ with checksums
make sign           # signed .update files for the binaries in dist/
make packages       # the rpm packages into dist/, and what the msi is built from
make docker         # the container image for linux/amd64, as go-fs:<version>
make viewer-deps    # the libraries of the document viewer, which every target above fetches first
make viewer-update  # pin the latest versions of those libraries, then fetch them
```

`make release` cross compiles go-fs for linux/amd64, darwin/arm64 and
windows/amd64, the platforms a release is built for. Builds use `CGO_ENABLED=0`,
so the binary carries no host dependencies (fully static on Linux and Windows;
on macOS it links only libSystem). Any other
platform Go supports builds with a plain `GOOS=… GOARCH=… go build .`.

`make docker` builds the container image from `packaging/docker/Dockerfile`.
`make docker-inputs` first puts all the build sees into `build/pkg/docker`: the
linux/amd64 binary stamped `-X main.packaging=docker`, which refuses to update
itself over HTTP and writes the starter configuration of the image, every
server listening on all interfaces, the entrypoint and the licence. The Dockerfile adds Alpine's ffmpeg, ffprobe and
deno and the standalone yt-dlp, checked against the checksums of its release;
`--build-arg YTDLP_VERSION=` picks another one. The release workflow builds the
image after publishing the release, with the latest yt-dlp, starts it once to
check that it answers, and pushes it to `ghcr.io/mailsvb/go-fs`.

## The libraries of the document viewer

The document viewer of the web interface (see [Viewing a file](BROWSER.md#viewing-a-file))
draws files with pdf.js, the Office Open XML Viewer, Ace, the editor text is
shown and edited in, and Video.js, which plays audio and video, which the
binary embeds.
They are not committed: `go run ./tools/viewer`, which `make viewer-deps` runs and
every build and test target of the Makefile runs first, fetches them from npm
into `internal/httpd/viewer/libs/`. Only `internal/httpd/viewer/libraries.lock` is
committed, a line per package with its version and the sha512 npm publishes for
its tarball:

```
pdfjs-dist 6.5.438 sha512-…
```

A tarball that does not match is refused, whether it was downloaded or comes
from the cache, so neither a mirror nor a cache can change what is embedded.
Tarballs are kept in `go-fs/npm` of the user's cache folder (`~/.cache` on
Linux, `~/Library/Caches` on macOS, `%LocalAppData%` on Windows), or wherever
`GOFS_NPM_CACHE` says, so a library is downloaded once per version.
`GOFS_NPM_REGISTRY` names a registry other than `https://registry.npmjs.org`, a
mirror for one; the proxy variables of the environment are honoured. A fetch
that is already there costs nothing.

`go run ./tools/viewer -update` (`make viewer-update`) pins the version npm tags
`latest` of each library, says what changed, and fetches it; an update is a
change to the lock file alone, which is reviewed as one. Video.js follows the
tag `latest-8` instead: the package npm tags `latest` is a stub of version 10,
which only throws, and version 10 is a different player in other packages.
Of Video.js only its core build is embedded, without the streaming of HLS and
DASH, since go-fs plays files as they are.
`go run ./tools/viewer -check` fails unless what is fetched is what the lock file
pins, which the release workflow runs before it builds.

A plain `go build .` before the fetch still builds and runs, without the viewer:
the listing shows files as it did before, and go-fs says why in its log when it
starts. Its tests skip what needs the libraries, unless `GOFS_REQUIRE_VIEWER` is
set, as it is in CI, where they fail instead. The tag `no_viewer` leaves the
viewer out altogether.

## Versioning

The released version lives in the `VERSION` file, which is the one place to bump
it. The two build targets stamp it differently:

| Target | Version stamped | Artifact |
|---|---|---|
| `make release` | `1.0.0` | `dist/go-fs_1.0.0_linux_amd64.update`, one per platform |
| `make build` | `1.0.0-dev-20260904-222134` | `dist/go-fs_1.0.0-dev-20260904-222134_linux_amd64`, one per platform |

Both cross compile the same binaries, one for each platform, into `dist/`, with checksums, after
emptying it. `make build` appends `-dev-<YYYYmmdd-HHMMSS>`, so a locally built
binary always says when it was built and can never be mistaken for a release.
Started without a configuration file, one writes the documented starter
configuration and serves from it.

With the signing key in `GOFS_SIGNING_KEY`, `make release` writes a signed
`dist/go-fs_1.0.0_linux_amd64.update` for each binary, the file a running go-fs
accepts as an update over HTTP (see [Updating over HTTP](INSTALL.md#updating-over-http)),
and keeps those: a release ships the installers and the `.update` files rather
than the plain binaries, but for `go-fs_1.0.0_darwin_arm64`, which has no
installer and so is the download for macOS. Without the key the binaries stay.
The release workflow refuses to run without it.

`go-fs -version` prints whichever version was stamped, and a plain `go build .`
reports `1.0.0-dev`. Pass `VERSION=` to override for a single build, for example
`make release VERSION=1.1` from a release pipeline.
