# go-fs

go-fileserver: FTP, FTPS, SFTP, HTTP, HTTPS and TFTP in a single statically
linked binary, configured from one TOML file, with hot reload and an optional
web interface for editing it. The FTP and TFTP servers are a
port of the Node.js [jsftpd](https://github.com/svenbeisiegel/jsftpd); the HTTP
server is a port of an Express one.

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

One TOML file with a `[general]`, a `[log]`, an `[ftp]`, an `[ftps]`, an
`[sftp]`, an `[http]`, an `[https]` and a `[tftp]` section. Every key is
optional and keeps the documented default when absent, so a working file can be
this short:

```toml
[general]
basefolder = "/srv/files"

[ftp]
port = 2121

[[ftp.users]]
username = "john"
password = "doe"

[tftp]
port = 6969
allowWrite = true
```

`general.basefolder` is the folder every server falls back to when its own
section does not name one, which is usually what you want — they all serve the
same tree. It has to be an absolute path and it has to exist. A section that
sets its own `basefolder` keeps it, so one server can be pointed somewhere else
without repeating the folder for the rest.

### Reloading

With `general.reloadConfig` on, which it is by default, the file is checked
every `reloadInterval` seconds and changes are applied without a restart. On
unix `kill -HUP` reloads on demand whether or not the watch is on.

What happens depends on what changed:

| Changed | Effect |
|---|---|
| accounts, permissions, paths, limits, timeouts, the cleanup list | applied to the running server, **nothing is dropped** — a download in flight finishes |
| a port, a `basefolder`, a TLS certificate, an SSH host key | that one server is restarted and its connections drop; the other four are untouched |
| a server switched on or off | it is started or stopped, the others untouched |

A file that does not parse or does not validate is reported at error level and
ignored, so a half-written save cannot take a server down — the last good
configuration keeps serving. The same goes for a change one server rejects, a
malformed authorized key say: that server keeps running as it was while the
others take the new file.

An account is checked again on every request, so a session cookie or a live
connection never outlives the rights it was granted by more than the request it
is in.

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

### The web interface

A browser interface for the whole file, off unless it is switched on:

```toml
[general]
adminInterfaceEnabled = true
adminInterfacePort = 10443
adminUsername = "admin"
adminPassword = "..."
```

It shows every section of the configuration as a tab, every repeated table —
`[[ftp.users]]`, `[[sftp.users]]`, `[[http.users]]`, `[[http.cleanup]]` — as a
list of records that can be added to and removed from, and each key with the
comment that documents it in `go-fs.example.toml`. One **Apply** button writes
the file; the watcher above then applies it, so the same rules hold — accounts
change without dropping anything, a port restarts one server.

The form is generated by reflecting over the configuration struct rather than
written out, so a key added to the file appears in the browser with nothing else
to do.

Three things to know before switching it on:

* **Apply rewrites the file through the TOML writer.** Every key is written out
  explicitly and the comments are lost. The previous file is kept beside it as
  `go-fs.toml.bak`, and the fully commented original is always `-init` away.
* **It binds `127.0.0.1` by default.** The page shows and edits every password
  in the file, so reach it through an SSH tunnel
  (`ssh -L 10443:127.0.0.1:10443 host`) or set `adminInterfaceAddress = ""` to
  bind every interface once you have thought about it.
* **It is served over TLS**, with a self-signed certificate generated at
  startup unless `adminCert` and `adminKey` hold a pair. A browser will warn
  about that certificate, and it is right to. Set
  `adminInterfaceUseHttps = false` to serve plain HTTP on the same port
  instead, which is for putting a reverse proxy that terminates TLS in front of
  it — with nothing in front, the admin password and every password on the page
  cross the network in the clear, and the server says so at startup when the
  address is not the loopback one.
* **The file it edits holds every private key.** That is what makes the upload
  below possible, and it is a reason to keep the file at `chmod 600`.

Certificates and keys are not typed in. Each of `ftps.cert`, `ftps.key`,
`https.cert`, `https.key`, `general.adminCert`, `general.adminKey` and
`sftp.hostkey` comes with:

* **Upload** — pick a `.pem`, `.crt` or `.key` file. The server parses it with
  the same code it uses at startup, so a file it accepts is a file it will start
  with, and refuses the rest by name: a key picked for a certificate box says
  so, and a leftover file path says what to write instead.
* **Generate** — makes a real self-signed pair, or a real host key, instead of
  the throwaway the server makes at every start. Generating a certificate fills
  its private key too. Because it is stored, it survives a restart, and clients
  stop reporting that the host key changed.
* a line under the box saying what is actually stored there — `certificate
  CN=example, expires 2027-01-01`, `ssh-ed25519 host key, SHA256:…` — since
  base64 on its own says nothing.

Neither button writes anything: the value lands on the page, and the single
Apply writes it.

Nothing is written until it would load: what is posted is validated exactly as
the file is at startup, so the page reports what `-check` would report and a
configuration that could not start is never written. A file that cannot be
written says so before anything is edited.

### The FTP data channel

FTP carries its data on a second connection, and which end opens it is the
client's choice. Both are always served; what the configuration decides is the
ports each one uses, which is what a firewall in front of the server has to be
told.

```toml
[ftp]
# passive (PASV, EPSV): the client connects in, on a port out of this range
passiveMinPort = 1024
passiveMaxPort = 1034
# active (PORT, EPRT): the server connects out, from this port
activeSourcePort = 0
```

**Passive** takes one port out of the range for the length of a transfer, so
the range should hold at least `maxConnections` ports; a narrower one is
allowed and warned about at startup, and a transfer that finds none free is
refused rather than straying outside it. Open the whole range in the firewall.

**Active** leaves from `activeSourcePort`. `0`, the default, lets the system
pick an ephemeral port, which a firewall cannot name; `20` is the port RFC 959
uses and what a firewall written for active FTP expects, though binding it on
unix needs the privilege for ports below 1024. The socket asks for
`SO_REUSEADDR`, so one fixed port still serves transfers running at the same
time and back to back, which would otherwise collide with the last connection's
`TIME_WAIT`.

All three apply to the next transfer, so changing them — in the file or in the
web interface — never drops a connection.

### Notable defaults

| Key | Default | Why |
|---|---|---|
| `ftp.allowFtpBounce` | `false` | `PORT`/`EPRT` may only name the connected client, otherwise the server can reach third parties on its behalf (RFC 2577) |
| `ftp.allowForeignDataConnection` | `false` | only the client that asked for a passive port may connect to it |
| `ftp.activeSourcePort` | `0` | the system picks the port an active data connection leaves from, since a fixed one below 1024 needs privilege |
| `ftp.idleTimeout` | `600` | an idle control connection does not hold a slot forever |
| `ftp.loginFailureDelay` | `1` | a wrong password is answered after a second, which slows guessing |
| `ftp.users[].allowUser*` | `false` | an account is granted only the rights its table lists |
| `sftp.enabled`, `http.enabled` | `false` | both are off by default, so an upgrade never opens a port on its own |
| `http.users[].allowUser*` | `false` | an account is granted only the rights its table lists |
| `tftp.allowWrite` | `false` | read only unless switched on |
| `tftp.maxBlockSize` | `1468` | keeps a block inside a typical ethernet MTU so datagrams are not IP fragmented |
| `tftp.maxTimeout` | `60` | a client cannot negotiate a retransmit interval that pins a transfer slot |
| `tftp.maxConnectionsPerHost` | `5` | one host cannot take every slot |

## TLS

Set `[ftps] enabled = true` to listen on `ftps.port` and offer `AUTH TLS` on the
plain port. `cert` and `key` hold the certificate and its private key
themselves, base64 of the PEM, the way `sftp.hostkey` does — not paths to files:

```shell
base64 < server.crt | tr -d "\n"    # into cert
base64 < server.key | tr -d "\n"    # into key
```

The web interface does this for you: press **Upload** beside either key and pick
the file. A whole chain in one file is kept whole, and a key protected by a
passphrase is refused, since there is nobody to ask for one at startup.

Keeping the material in the file means the configuration is one file that can be
copied to another host, and go-fs needs no read access outside it. It also means
the file holds private keys as base64, which is *encoding, not encryption*: it
deserves the permissions a private key deserves, `chmod 600` and an owner that
is not shared.

`-check` decodes both halves and matches them against each other, so a truncated
paste, a key put into the certificate key, or a key belonging to a different
certificate is reported by name before the server tries to serve it.

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

The host key lives in the configuration itself rather than in a separate file,
as every certificate and key here does: `hostkey` is base64 of its PEM encoding,
on one line. With it empty a key is generated at every start, which makes every
client report a changed host key, so set it for anything but a first look —
with **Generate** in the web interface, or by hand:

```shell
ssh-keygen -q -t ed25519 -N "" -f hostkey && base64 < hostkey | tr -d "\n"
```

## HTTP

`[http]` serves the folder over HTTP: `GET` browses and downloads, `PUT`
uploads, `DELETE` removes. `[https]` is the same server on a TLS port, with
`cert` and `key` holding the material as in `[ftps]`, and the two `enabled`
switches are independent.

Access has two layers, which is what the Express server it replaces did:

```toml
[http]
enabled = true
port = 9080
basefolder = "/srv/http"
methodsRequireAuth = ["PUT", "DELETE", "POST"]
pathsRequireAuth = ["^/private/.*"]

[[http.users]]
username = "john"
password = "doe"
paths = ["^/private/.*"]
allowUserFileUpload = true
allowUserFileDelete = true
cookie = true
```

A request is **public** unless its method is in `methodsRequireAuth` or its path
matches one of `pathsRequireAuth`. Anything else has to be answered by an
account, and that account's own `paths` then decide what it may reach:
`allowUserFileUpload` for `PUT`, `allowUserFileDelete` for `DELETE`, both false
unless set. A path an account may not reach is `403`, not another challenge.

`paths` are matched against the request path **after** it has been normalized,
so `/private/../secret` is tested as `/secret` and cannot be used to slip past a
pattern.

Both Digest and Basic authentication are accepted. The challenge offers Digest,
with SHA-256 for Chromium and Firefox and MD5 for everything else, which is what
those clients handle; `realm` is hashed into the response, so changing it makes
browsers ask again.

With `cookie = true` an account is handed a session cookie once it has
authenticated, so a browser stops repeating the credentials. The session names
the account, and its `paths` and rights are checked again on every request — a
session can never reach further than the account behind it. `cookiePath` only
tells the browser which URLs to send it back for. A client that sends
`X-Disable-Session` is never given one.

`[[http.cleanup]]` keeps a folder from growing without bound: once an hour
everything but the newest `keep` files in it is removed. It is the one thing in
go-fs that deletes without a client asking, so every removal is logged.

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

**HTTP** — `GET` for downloads and a browsable listing, `PUT` for
`application/octet-stream` and multipart uploads, `DELETE` for a file or an
empty folder, Basic (RFC 7617) and Digest (RFC 7616, with the RFC 2069 form)
authentication, session cookies, and the legacy `dls_directory_reader` listing
endpoint. Downloads answer range requests, so a large one can be resumed.

**TFTP** — RFC 1350 in `octet` and `netascii` mode, with the option extension of
RFC 2347, the `blksize`, `timeout` and `tsize` options of RFC 2348 and RFC 2349,
and windowed reads per RFC 7440.

