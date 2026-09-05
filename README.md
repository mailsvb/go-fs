# go-fs

go-fileserver: FTP, FTPS, SFTP and TFTP in a single statically linked binary,
configured from one TOML file. The FTP and TFTP servers are a port of the
Node.js [jsftpd](https://github.com/svenbeisiegel/jsftpd).

## Build

Requires Go 1.27 or newer.

```shell
make build          # the same five binaries, stamped as a dev build
make test           # the test suite
make race           # the test suite under the race detector
make release        # all five release binaries into dist/ with checksums
```

`make release` cross compiles for linux and windows on amd64 and arm64, and for
macOS on arm64. Builds use `CGO_ENABLED=0`, so the binary carries no host
dependencies: on Linux and Windows it is fully static, on macOS it links only
`libSystem` as the operating system requires.

## Versioning

The released version lives in the `VERSION` file, which is the one place to bump
it. The two build targets stamp it differently:

| Target | Version stamped | Artifact |
|---|---|---|
| `make release` | `0.9` | `dist/go-fs_0.9_linux_amd64`, one per platform |
| `make build` | `0.9-dev-20260904-222134` | `dist/go-fs_0.9-dev-20260904-222134_linux_amd64`, one per platform |

The two targets differ only in the version they stamp: both cross compile the
same five binaries into `dist/`, with checksums, after emptying it. `make build`
appends `-dev-<YYYYmmdd-HHMMSS>`, so a locally built binary always says when it
was built and can never be mistaken for a release.

Both also copy the documented starter configuration into `dist/go-fs.toml`,
which is the name the binary reads when `-config` is not given, so an unpacked
`dist/` is ready to edit and run. It is the same file the binary embeds and
`-init` writes, so the shipped configuration always matches the build.

`go-fs -version` prints whichever version was stamped, and a plain `go build .`
reports `0.9-dev`. Pass `VERSION=` to override for a single build, for example
`make release VERSION=1.0` from a release pipeline.

## Run

```shell
go-fs -init /etc/go-fs.toml     # write a documented starter configuration
$EDITOR /etc/go-fs.toml         # set the folders and the accounts
go-fs -config /etc/go-fs.toml -check   # report configuration problems
go-fs -config /etc/go-fs.toml
```

| Flag | Meaning |
|---|---|
| `-config <path>` | configuration file, `go-fs.toml` by default |
| `-init <path>` | write a documented starter configuration and exit |
| `-check` | load the configuration, report problems and exit |
| `-version` | print the version and exit |

`SIGINT` and `SIGTERM` shut the servers down, dropping open connections and
aborting running transfers.

Serving on ports 21 and 69 needs privileges. Prefer a service manager that binds
them for you, or run on high ports behind a redirect.

## Configuration

One TOML file with a `[log]`, an `[ftp]`, an `[ftps]`, an `[sftp]` and a
`[tftp]` section. Every key is optional and keeps the documented default when
absent, so a working file can be this short:

```toml
[ftp]
port = 2121
basefolder = "/srv/ftp"

[[ftp.users]]
username = "john"
password = "doe"

[tftp]
port = 6969
basefolder = "/srv/tftp"
allowWrite = true
```

`go-fs.example.toml` is the fully commented version, and the same file
`-init` writes. Accounts are one `[[ftp.users]]` table each; there is no default
account, so a name that is not listed cannot log in. Each user may have its own
`basefolder`. `[sftp]` has the same shape with its own `[[sftp.users]]`, and
its accounts follow the same rules.

Every right an account has is granted explicitly — `allowUserFileCreate`,
`allowUserFileRetrieve`, `allowUserFileOverwrite`, `allowUserFileDelete`,
`allowUserFolderCreate` and `allowUserFolderDelete` all deny when they are not
set, so an account that lists none of them can log in and look around and
nothing more.

Anonymous access is not a setting of its own, just an account that takes no
password:

```toml
[[ftp.users]]
username = "anonymous"
password = ""
allowLoginWithoutPassword = true
allowUserFileRetrieve = true
```

Both servers confine every request to their base folder. Symbolic links are
resolved before that check, so a link inside the folder cannot be used to reach
out of it.

### Notable defaults

| Key | Default | Why |
|---|---|---|
| `ftp.allowFtpBounce` | `false` | `PORT`/`EPRT` may only name the connected client, otherwise the server can reach third parties on its behalf (RFC 2577) |
| `ftp.allowForeignDataConnection` | `false` | only the client that asked for a passive port may connect to it |
| `ftp.idleTimeout` | `600` | an idle control connection does not hold a slot forever |
| `ftp.loginFailureDelay` | `1` | a wrong password is answered after a second, which slows guessing |
| `ftp.users[].allowUser*` | `false` | an account is granted only the rights its table lists |
| `sftp.enabled` | `false` | the only server that is off by default, so an upgrade never opens an SSH port on its own |
| `tftp.allowWrite` | `false` | read only unless switched on |
| `tftp.maxBlockSize` | `1468` | keeps a block inside a typical ethernet MTU so datagrams are not IP fragmented |
| `tftp.maxTimeout` | `60` | a client cannot negotiate a retransmit interval that pins a transfer slot |
| `tftp.maxConnectionsPerHost` | `5` | one host cannot take every slot |

## TLS

Set `[ftps] enabled = true` to listen on `ftps.port` and offer `AUTH TLS` on the
plain port. Point `cert` and `key` at your PEM files.

`[ftps]` is a section of its own only because TOML tables are top level: it
configures the same server, which serves the folders, accounts and limits of
`[ftp]` on both listeners. The two `enabled` switches are independent, so
`ftp.enabled = false` with `ftps.enabled = true` serves implicit FTPS with
nothing on the plaintext port.

With `cert` and `key` empty the server generates a self-signed certificate at
startup and says so. That certificate changes on every restart and proves no
identity; it is there so the TLS interface works out of the box for a test, not
for production.

## SFTP

SFTP is the file transfer subsystem of SSH, so `[sftp]` runs an SSH server. It
serves only that subsystem: a `shell` or `exec` request is refused, and there is
no way to run anything on the host through it.

```toml
[sftp]
enabled = true
port = 2222
basefolder = "/srv/sftp"
hostkey = ""

[[sftp.users]]
username = "john"
password = "doe"
allowUserFileRetrieve = true

[[sftp.users]]
username = "max"
authorizedKeys = ["ssh-ed25519 AAAAC3Nz... max@laptop"]
allowUserFileRetrieve = true
allowUserFileCreate = true
```

An account authenticates with a password, with a public key, or with either
when both are configured. `authorizedKeys` entries are `authorized_keys` lines,
the content of an `id_*.pub` file. `allowLoginWithoutPassword` means nothing
here — SSH has no anonymous login — so an account needs a password or a key, and
one with neither is refused at startup rather than left unusable.

The host key lives in the configuration itself rather than in a separate file:
`hostkey` is base64 of its PEM encoding, on one line. With it empty a key is
generated at every start, which makes every client report a changed host key, so
set it for anything but a first look:

```shell
ssh-keygen -q -t ed25519 -N "" -f hostkey && base64 < hostkey | tr -d "\n"
```

## What is implemented

**FTP** — RFC 959 with RFC 2228 (`AUTH`, `PBSZ`, `PROT`), RFC 2389 (`FEAT`,
`OPTS`), RFC 2428 (`EPRT`, `EPSV`, `EPSV ALL`), RFC 3659 (`MLST`, `MLSD`,
`MDTM`, `SIZE`, `REST`) and the RFC 775 `X` aliases:

```
ABOR ACCT ALLO APPE AUTH CDUP CLNT CWD DELE EPRT EPSV FEAT HELP LIST MDTM MFMT
MKD MLSD MLST MODE NLST NOOP OPTS PASS PASV PBSZ PORT PROT PWD QUIT REST RETR
RMD RMDA RNFR RNTO SITE SIZE STAT STOR STOU STRU SYST TYPE USER XCUP XCWD XMKD
XPWD XRMD
```

`SITE CHMOD` and `SITE HELP` are the implemented `SITE` subcommands.

**SFTP** — version 3 of the SFTP protocol over SSH, through
`golang.org/x/crypto/ssh` and `github.com/pkg/sftp`: open, read, write, append,
truncate, directory listing, stat, rename, remove, mkdir, rmdir and setstat.
Creating symbolic links is refused, because a link is the one thing that could
point out of the base folder.

**TFTP** — RFC 1350 in `octet` and `netascii` mode, with the option extension of
RFC 2347, the `blksize`, `timeout` and `tsize` options of RFC 2348 and RFC 2349,
and windowed reads per RFC 7440.

## Differences from the Node implementation

* The programmatic handler hooks (`hdl.upload`, `hdl.download`, `hdl.list`,
  `hdl.rename`) are gone. This is a configurable binary, not a library, so both
  servers always work on the file system.
* No certificate is embedded. The Node package ships one whose private key is
  published with it; here a certificate is generated instead, see above.
* The EventEmitter events became structured log records: `login`, `logoff`,
  `download` and `upload` are logged at info level with their attributes, the
  protocol trace at debug level.
* `basefolder` has to exist. There is no auto-created default folder and no
  `cleanup()`.
* `tftp.type` means what it says: empty binds dual stack, `udp4` binds IPv4 only
  and `udp6` binds IPv6 only.

Everything else, down to the reply strings, matches the Node implementation.
