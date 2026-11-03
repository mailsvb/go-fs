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
```

`make release` cross compiles go-fs and go-fs-light for linux on amd64 and
arm64 and for windows on amd64, the platforms a release has a package for. Builds use `CGO_ENABLED=0`,
so the binary is fully static and carries no host dependencies. Any other
platform Go supports builds with a plain `GOOS=… GOARCH=… go build .`.

## Versioning

The released version lives in the `VERSION` file, which is the one place to bump
it. The two build targets stamp it differently:

| Target | Version stamped | Artifact |
|---|---|---|
| `make release` | `1.0.0` | `dist/go-fs_1.0.0_linux_amd64.update`, one per platform |
| `make build` | `1.0.0-dev-20260904-222134` | `dist/go-fs_1.0.0-dev-20260904-222134_linux_amd64`, one per platform |

Both cross compile the same binaries, go-fs and go-fs-light for each platform, into `dist/`, with checksums, after
emptying it. `make build` appends `-dev-<YYYYmmdd-HHMMSS>`, so a locally built
binary always says when it was built and can never be mistaken for a release.
`go-fs -init` writes the documented starter configuration next to one.

With the signing keys in `GOFS_SIGNING_KEY` and `GOFS_LIGHT_SIGNING_KEY`,
`make release` writes a signed `dist/go-fs_1.0.0_linux_amd64.update` for each
binary, the file a running go-fs
accepts as an update over HTTP (see [Updating over HTTP](INSTALL.md#updating-over-http)),
and keeps only those: a release ships the installers and the `.update` files,
not the plain binaries. Without the keys the binaries stay. The release
workflow refuses to run without both.

`go-fs -version` prints whichever version was stamped, and a plain `go build .`
reports `1.0.0-dev`. Pass `VERSION=` to override for a single build, for example
`make release VERSION=1.1` from a release pipeline.

## go-fs-light

go-fs-light is a second product built from the same code with the build tag
`light`. It leaves out every protocol but HTTP, HTTPS, the S3 API and the
container registry, and the vaults of Cryptomator; neither the code of what is
left out nor the modules it imports end up in the binary. Its files are named
after it, its updates are signed with a key of their own
(`GOFS_LIGHT_SIGNING_KEY`), and it installs to the same paths as go-fs.

Each optional part can also be left out on its own, with one of the tags
`no_ftp`, `no_sftpd`, `no_sftp_remote`, `no_smb`, `no_tftp`, `no_webdav`,
`no_rpm`, `no_fdroid`, `no_jellyfin` and `no_cryptomator`
(see `internal/features`). The Features workflow vets and tests go-fs-light
and checks the modules it links against `tools/features/light-modules.txt`.
