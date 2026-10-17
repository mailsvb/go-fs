# go-fs threat model

go-fs is a file server in one statically linked Go binary: FTP and FTPS, SFTP,
HTTP and HTTPS with a browser interface, an S3 compatible API, an OCI container
registry, rpm and F-Droid package repositories, and TFTP, all configured from
one TOML file that an admin web interface can edit. It is run on servers and
reachable from untrusted networks, so every listener takes untrusted input.
README.md documents the behaviour each server promises; treat a deviation from
it that crosses a trust boundary as a finding.

## Who is trusted

- **Untrusted:** any network client before it authenticates, on every
  listener; an authenticated account acting beyond the switches (`ftp`, `sftp`,
  `http`, `s3`, `registry`) and rights (`allowUser*`, `paths`) its `[[users]]`
  entry grants; a bearer token beyond its scope; a holder of a share link
  beyond the one file it names; content fetched from remote servers (fetch from
  URL, registry pulls, Artifactory, SFTP/SMB hosts); files uploaded by
  accounts (archives, images, rpms, APKs) when go-fs parses them.
- **Trusted:** the configuration file and whoever can write it, the operating
  system account go-fs runs as, the host, and accounts that set `isAdmin` once
  logged in through the browser (they can rewrite the configuration by
  design). Do not report what an admin can do through the admin interface.

## Security properties that matter most

- **Confinement to the base folder** (`internal/vfs`): no path from any
  protocol may read, write, list, rename or delete outside the account's base
  folder or its `paths`. Consider `..`, absolute paths, symlinks (also ones
  created between check and use), case folding, Unicode normalisation, Windows
  separators, drive letters and reserved names, NUL bytes, and archive entries
  (folder download, registry imports). The base folder itself must not be
  renamed or removed.
- **Authentication and authorisation** on every server: passwords (FTP `USER`/
  `PASS`, SFTP, HTTP Basic and Digest), SSH public keys, browser sessions (JWT
  signed with `httpSessionTokenSecret`), bearer tokens, S3 Signature V4 (header,
  presigned, `aws-chunked`), registry tokens from `/v2/_token`, share links
  (HMAC with `shareLinkSecret`). Look for bypasses, confusion between
  credential kinds (the admin interface and self-update refuse Basic/Digest),
  algorithm or signature confusion, timing leaks (`internal/secrets`), login
  rate limiting and lockout bypass, and `trustedProxies` spoofing of the client
  address. Every operation must need exactly the rights in README's table.
- **Self-update** (`internal/selfupdate`, `internal/httpd/update.go`): an
  `.update` file must be accepted only with a valid ed25519 signature by a key
  the binary trusts; anything that installs or executes unsigned or
  foreign-key code is critical.
- **Server-side requests** (`internal/httpd/fetch.go`, `send.go`,
  `remote_*.go`, `registry_remote.go`, `internal/remote`): fetching from a URL
  deliberately reaches any host, internal ones included, for accounts that may
  create files; that by itself is not a finding. Credentials and
  `Authorization` headers leaking to another origin on redirect, header
  injection, reaching these features without the required right or without a
  session, and FTP bounce (`PORT`/`EPRT` to a third party while
  `allowFtpBounce` is off, or foreign data connections) are.
- **Browser interface**: XSS (file names, listings, registry pages, error
  messages), CSRF against state-changing endpoints (`?go-fs=…` actions, the
  admin config editor), open redirects, and session fixation.
- **Secrets at rest**: stored server passwords and Cryptomator vaults
  (`internal/cryptomator`, in go-fs only) must not be readable without their keys; secrets in
  the configuration must not leak into logs, listings or API answers.
- **Parsers of untrusted data**: FTP and TFTP commands and options, SFTP
  packets, S3 XML and chunked bodies, multipart uploads, OCI manifests and
  archives (`registry_*.go`), rpm headers (`internal/rpm`), APKs
  (`internal/fdroid`), and the TOML config written by the admin interface.
  Panics, unbounded memory or disk use, and goroutine or connection leaks one
  client can trigger are denial of service.

## Less important / out of scope

- `tools/`, `packaging/`, the Makefile and `.github/` are build and release
  tooling, not shipped code.
- TFTP has no authentication by protocol; anonymous access within `allowRead`
  and `allowWrite` is intended. Escaping its folder is still a finding.
- Anonymous registry pulls while `registryAnonymousRead` is on, and public
  HTTP paths the configuration makes public, are intended.
- Weak settings an operator chooses (plain FTP or HTTP, `skipVerify`, a default
  password the server warns about) are not findings.
- Third-party modules only where a flaw is reachable through go-fs.

## How to exercise it

- The checkout is at `/src`. The binaries are `/usr/local/bin/go-fs` and
  `/usr/local/bin/go-fs-light` (built with `-tags light`: HTTP, HTTPS, S3 and the
  registry only, without Cryptomator vaults; see `internal/features`).
- `go-fs -init` writes the documented starter configuration
  (`internal/config/template.toml`); `go-fs.example.toml` shows every key.
  `go-fs -check -config FILE` validates one. Servers listen on 127.0.0.1 and
  ports above 1024 by default.
- Each package has unit tests; `internal/e2e` runs the HTTP, FTP and SFTP
  servers side by side over one folder and is the best harness for
  cross-protocol reproducers. Fuzz targets live in `internal/config`,
  `internal/vfs`, `internal/tftp`, `internal/httpd`, `internal/secrets` and
  `internal/cryptomator`.
- The baseline is `go test -count=1 -p 2 ./...`. `internal/httpd` tests can
  hang or fail when many test binaries run in parallel; rerun that package
  alone before treating a failure there as real. The `internal/fdroid` check
  with `jarsigner` skips, as no JDK is installed.
- Go, the module cache and the build cache are in the image;
  `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOSUMDB=off` keep builds offline.

## How we rate severity

- **Critical:** unauthenticated code execution; an unauthenticated
  authentication bypass; reading or writing outside the base folder; installing
  an unsigned or wrongly signed update; reaching the admin interface or
  rewriting the configuration without an admin session.
- **High:** an authenticated account exceeding its rights or `paths`, or
  reaching another account's files; credential or secret disclosure; XSS or
  CSRF that acts as another user; SSRF or FTP bounce outside the documented,
  rights-gated features.
- **Medium:** denial of service by one unauthenticated client (crash, memory or
  disk exhaustion, holding every slot); login rate limit bypass; information
  disclosure of file names or metadata outside an account's scope.
- **Low:** the same needing an authenticated account, or a non-default
  configuration that README warns about.

## Reports

Please include a reproducer as a Go test in the affected package (or in
`internal/e2e` when it spans servers) and a minimal patch in the style of the
surrounding code.
