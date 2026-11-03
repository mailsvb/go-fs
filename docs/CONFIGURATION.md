# Configuration

One TOML file with a `[general]` section, its `[general.ssh]` table and
`[[general.cleanup]]` list, a `[[users]]` list, an `[ftp]`, an `[ftps]`, an `[sftp]`, an `[http]`, an
`[https]` and a `[tftp]` section.
Every key is optional and keeps the documented default when absent, so a
working file can be this short:

```toml
[general]
baseFolderFiles = "/var/lib/go-fs/files"

[[users]]
username = "john"
password = "doe"
ftp = true

[ftp]
enabled = true
address = ""
port = 2121

[tftp]
enabled = true
port = 6969
allowWrite = true
```

Without a section, a server keeps its defaults: only HTTP is on, every server
listens on `127.0.0.1` alone, and the ports are 8021 for FTP, 8990 for FTPS,
8022 for SFTP, 8080 for HTTP, 8443 for HTTPS and 8069 for TFTP. `address = ""`
listens on every interface.

`general.baseFolderFiles` is the folder every server serves: FTP, SFTP, HTTP,
S3, WebDAV and TFTP all see the same tree. `general.baseFolderRegistry` is the
folder of the [container registry](REGISTRY.md#container-registry), which is off while it
is empty. `general.baseFolderTemp` is where an upload to the HTTP server is
staged until it is whole — a chunked upload, an S3 object, a WebDAV file or a
fetch — and a `go-fs-uploads` folder under the OS's temp directory while it is
empty; it must not overlap either of the other two or an account's folder,
and belongs on the same filesystem as `general.baseFolderFiles` so finishing
an upload is a rename. `general.baseFolderTrash` is where the HTTP server puts
what it deletes, see [The trash](BROWSER.md#the-trash); it is off while it is empty,
must not overlap any of the others, and belongs on the same filesystem as
`general.baseFolderFiles` as well. These are the only folders: no server has a
folder of its own, and only an account can be pointed elsewhere, see
[Accounts](#accounts).

The starter file keeps all four in one place:

```toml
[general]
baseFolderFiles = "/var/lib/go-fs/files"
baseFolderRegistry = "/var/lib/go-fs/registry"
baseFolderTemp = "/var/lib/go-fs/temp"
baseFolderTrash = "/var/lib/go-fs/trash"
trashRetentionDays = 30
```

Each has to be an absolute path. One that is not there is made when go-fs
starts, reloads the file or saves it from the admin interface, with every
folder above it, as `mkdir -p` would: it belongs to the user go-fs runs as,
with mode `0750` for the files and the registry and `0700` for the temp
folder and the trash, and `0755` for a folder made above them. A folder that is there
already is left as it is. go-fs has to be allowed to write to the folder it
makes them in, and `/var/lib` belongs to root: the rpm makes `/var/lib/go-fs`
for the go-fs account it runs as, and the msi makes `C:\ProgramData\go-fs`.
Run by hand as another account, make the folder once and hand it over,
`sudo install -d -o <user> -g <group> -m 0750 /var/lib/go-fs`, or point the
four keys at a folder that account owns.

A file written for an older version names them `general.basefolder`,
`http.registryBaseFolder` and `http.uploadStagingFolder`. It is still read,
and go-fs renames the keys in it at the next start, keeping the comments and
the file as it was beside it as `go-fs.toml.bak`. A `basefolder` in `[ftp]`,
`[sftp]`, `[http]` or `[tftp]` is no longer read: it is reported at startup and
removed by the same rename.

`[[general.cleanup]]` keeps a folder from growing without bound: once an hour
everything but the newest `keep` files in it is removed. Paths are relative to
`general.baseFolderFiles`, and the sweep runs whichever servers are enabled. It
is the one thing in go-fs that deletes without a client asking, so every
removal is logged.

```toml
[[general.cleanup]]
path = "/iso"
keep = 10
```

## Reloading

With `general.reloadConfig` on, which it is by default, the file is checked
every `reloadInterval` seconds and changes are applied without a restart. On
unix `kill -HUP` reloads on demand whether or not the watch is on.

What happens depends on what changed:

| Changed | Effect |
|---|---|
| accounts, permissions, paths, limits, timeouts, the cleanup list | applied to the running server, **nothing is dropped** — a download in flight finishes |
| a port, a TLS certificate, an SSH host key | that one server is restarted and its connections drop; the other four are untouched |
| `general.baseFolderFiles` | every server is restarted |
| `general.baseFolderTemp` | applied to the next upload, nothing is restarted |
| `general.baseFolderTrash`, `general.trashRetentionDays` | applied to the next delete and the next hourly sweep, nothing is restarted |
| a server switched on or off | it is started or stopped, the others untouched |

A file that does not parse or does not validate is reported at error level and
ignored, so a half-written save cannot take a server down — the last good
configuration keeps serving. The same goes for a change one server rejects, a
malformed authorized key say: that server keeps running as it was while the
others take the new file.

An account is checked again on every request, so a browser session or a live
connection never outlives the rights it was granted by more than the request it
is in.

`go-fs.example.toml` is the fully commented version, and the same file
`-init` writes.

## Accounts

Accounts are one `[[users]]` table each, and every server draws from the same
list: an account is configured once, with one password, and says which servers
it may log in to with `ftp = true`, `sftp = true` and `http = true`, whether
it may use the [S3 API](PROTOCOLS.md#s3) with `s3 = true`, and whether it may push to the
[container registry](REGISTRY.md#container-registry) with `registry = true`, each off
unless set. There is no built-in account, so a name that is not listed cannot
log in anywhere (the shipped configuration lists one, `admin`, see
[Install](INSTALL.md#install)), and a name is listed once — the same person on FTP and HTTP is
one entry with both switches on. An entry that switches nothing on is kept
without being served, which is the way to park an account.

Each account may have its own `basefolder`, which FTP and SFTP serve instead of
`general.baseFolderFiles`; HTTP scopes an account by `paths` instead. `allowLoginWithoutPassword`
is read by FTP alone, `authorizedKeys` by SFTP alone and `paths` by HTTP and S3;
the servers a key does not apply to ignore it. The container registry reads
neither `paths` nor the rights below: `registry = true` is the whole of what an
account may do there.

**The shipped file defines no account.** The examples in it are commented out on
purpose, so a fresh configuration serves nobody until you put a name and a
password of your own in — rather than starting life with the ones printed on
this page. A password that is still one of those is reported at warning level
every time the server starts, as is a configuration file that more than its
owner can read.

Every right an account has is granted explicitly — `allowUserFileCreate`,
`allowUserFileRetrieve`, `allowUserFileOverwrite`, `allowUserFileDelete`,
`allowUserFolderCreate` and `allowUserFolderDelete` all deny when they are not
set, so an account that lists none of them can log in and look around and
nothing more. The rights are one set that holds on every server the account
uses: `allowUserFileDelete = true` lets it delete over FTP, SFTP and HTTP alike.

An operation that does two things needs both rights, and no right reaches
further than its name says:

| Operation | Needs |
|---|---|
| `RMD`, `XRMD`, SFTP `rmdir`, HTTP `DELETE` of a folder | `allowUserFolderDelete`, and the folder has to be empty |
| `RMDA` | `allowUserFolderDelete` **and** `allowUserFileDelete`, since what it removes is files |
| `RNFR`/`RNTO`, SFTP rename, HTTP `MOVE` | `allowUserFileCreate` **and** `allowUserFileDelete`: a rename makes one name and unmakes another |
| `MFMT`, `SITE CHMOD`, SFTP setstat | `allowUserFileOverwrite`, since they change the file |
| HTTP `PUT` of a new name | `allowUserFileCreate` |
| HTTP `PUT` over a file that exists | `allowUserFileOverwrite`; without it the name is taken |
| HTTP `MKCOL` | `allowUserFolderCreate` |
| HTTP `DELETE` of a file | `allowUserFileDelete` |
| HTTP `GET`, `HEAD` and the listing, where an account is required | `allowUserFileRetrieve` — where the request is public no account is asked |
| S3 `GetObject`, `HeadObject`, the listings | `allowUserFileRetrieve`; a listing leaves out what `paths` do not reach |
| S3 `PutObject`, and completing a multipart upload | `allowUserFileCreate` for a new key, `allowUserFileOverwrite` for one that exists |
| S3 `PutObject` of a key ending in `/` | `allowUserFolderCreate` |
| S3 `DeleteObject` | `allowUserFileDelete`, or `allowUserFolderDelete` for a key ending in `/` |
| S3 `CopyObject`, `UploadPartCopy` | `allowUserFileRetrieve` on the source, and create or overwrite on the target |
| Registry push, tag and delete | `registry`; the rights above do not apply |
| Registry pull | nothing while `http.registryAnonymousRead` is on, `registry` when it is off |

Anonymous access is not a setting of its own, just an account that takes no
password:

```toml
[[users]]
username = "anonymous"
password = ""
ftp = true
allowLoginWithoutPassword = true
allowUserFileRetrieve = true
```

**Upgrading from a file with `[[ftp.users]]`, `[[sftp.users]]` or
`[[http.users]]`:** those tables are not read any more. The file still loads,
with no accounts, and the server says so at warning level on every start. Move
each entry under `[[users]]`, add the switch of the server it was listed under,
and for an HTTP account replace `allowUserFileUpload = true` with
`allowUserFileCreate = true` and `allowUserFolderCreate = true`, and add
`allowUserFileRetrieve = true` if it reads paths that require an account.

Every server confines every request to its base folder. Symbolic links are
resolved before that check, so a link inside the folder cannot be used to reach
out of it, and the base folder itself can never be renamed or removed by a
client.

## The web interface

A browser interface for the whole file. It has no listener of its own: the HTTP
server serves it, at `?go-fs=admin` on any folder it lists, to an account that
sets `isAdmin` and has logged in through the browser.

```toml
[[users]]
username = "root"
password = "..."
http = true
isAdmin = true

[http]
enabled = true
enableAdminInterface = true   # the default
```

Log in on the listing page as that account and an **Admin** button appears in
the header, next to **Log out**; it leads to `https://host:8443/?go-fs=admin`
(the listing also turns `/#/admin` into that address). Nobody else is answered:
a browser with no session is sent to the login page, a session of an account
that does not set `isAdmin` is `403`, and the account's password sent as a
Basic or Digest header is `401` — the interface takes a session established
through the form and nothing else. `enableAdminInterface = false` switches it
off, which a reload applies without a restart.

It shows every section of the configuration as a tab, with the accounts on a
**USERS** tab of their own between GENERAL and FTP, and every repeated table —
`[[users]]`, `[[general.cleanup]]` — as a list of records that can be added to and
removed from. A table nested in a section, `[general.ssh]`, gets a row of tabs
of its own under the section's: **main** for the section's own keys, then one
per nested table and per repeated list, so GENERAL shows **main**, **ssh** and **cleanup**. Each record folds up to one line naming it, `john · ftp, http`,
and starts folded, so a long account list reads as a list of names and one
opens to be edited. Each key comes with the comment that documents it in
`go-fs.example.toml`. One **Apply** button writes
the file; the watcher above then applies it, so the same rules hold — accounts
change without dropping anything, a port restarts one server.

The form is generated by reflecting over the configuration struct rather than
written out, so a key added to the file appears in the browser with nothing else
to do.

Three things to know about it:

* **Apply rewrites the file through the TOML writer.** Every key is written out
  explicitly and the comments are lost. The previous file is kept beside it as
  `go-fs.toml.bak`, and the fully commented original is always `-init` away.
* **It is as exposed as the HTTP server is.** The page shows and edits every
  password in the file, so serve it over `[https]`: with `http.enabled = true`
  on an address other than the loopback one, the admin session and every
  password on the page cross the network in the clear, and the server says so
  at startup. Keep the plain port on `127.0.0.1`, or behind a proxy that
  terminates TLS and is listed in `http.trustedProxies`, or off.
* **The file it edits holds every private key.** That is what makes the upload
  below possible, and it is a reason to keep the file at `chmod 600`.

**Upgrading from a file with `general.adminInterfaceEnabled`,
`adminInterfacePort`, `adminUsername` and the other `admin*` keys:** they are
not read any more, and the server says so at warning level on every start.
Give one of the `[[users]]` entries `http = true` and `isAdmin = true`
instead, and reach the interface through the HTTP or HTTPS
port; the old separate listener is gone, and `adminCert`/`adminKey` are what
`https.cert`/`https.key` are for.

Certificates and keys are not typed in. Each of `ftps.cert`, `ftps.key`,
`https.cert`, `https.key` and `sftp.hostkey` comes with:

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

## Bind addresses and stalled transfers

`ftp.address`, `sftp.address`, `http.address` and `tftp.address` name the
interface each server binds to. They default to `127.0.0.1`, which only the
machine itself can reach; empty binds to all interfaces. The FTP one
covers the passive data ports too, so they follow the control port onto the same
interface.

`ftp.transferIdleTimeout`, 300 seconds by default, is how long a running
transfer may move nothing before it is dropped. It bounds a stall rather than
the length of a transfer, so a download of any size finishes as long as it keeps
moving, while a client that opens the data connection and then stops reading
gives its connection slot back instead of holding one until it disconnects.
`idleTimeout` does not apply while a transfer is running: a client busy on the
data connection owes nothing on the control one.

## Notable defaults

| Key | Default | Why |
|---|---|---|
| `ftp.allowFtpBounce` | `false` | `PORT`/`EPRT` may only name the connected client, otherwise the server can reach third parties on its behalf (RFC 2577) |
| `ftp.allowForeignDataConnection` | `false` | only the client that asked for a passive port may connect to it |
| `ftp.activeSourcePort` | `0` | the system picks the port an active data connection leaves from, since a fixed one below 1024 needs privilege |
| `ftp.idleTimeout` | `600` | an idle control connection does not hold a slot forever, though a running transfer is never idle |
| `ftp.transferIdleTimeout` | `300` | a transfer that stalls gives its slot back, however long a moving one takes |
| `ftp.loginFailureDelay` | `1` | a wrong password is answered after a second, which slows guessing |
| `http.loginAttempts`, `http.loginLockout` | `5`, `60` | an address that sends five wrong passwords in a minute is refused for a minute, at once |
| `http.trustedProxies` | `[]` | a forwarded address, and a forwarded `https`, is believed only from a proxy listed here |
| `users[].ftp`, `users[].sftp`, `users[].http`, `users[].s3` | `false` | an account logs in only to the servers it switches on |
| `http.enableS3` | `true` | the S3 API is there wherever the HTTP server is, and serves only the accounts that set `s3` |
| `http.jellyfinDiscovery` | `false` | answering broadcasts opens a UDP port on every interface, so being found on the network is switched on deliberately |
| `http.webdav` | `false` | a WebDAV client deletes folders recursively and moves across them, so mounting the folder as a drive is switched on deliberately |
| `users[].allowUser*` | `false` | an account is granted only the rights its table lists |
| `http.enabled` | `true` | the one server that is on, so the admin interface can be reached to set up the rest |
| `ftp.enabled`, `ftps.enabled`, `sftp.enabled`, `https.enabled`, `tftp.enabled` | `false` | each server is switched on deliberately |
| `ftp.address`, `sftp.address`, `http.address`, `tftp.address` | `127.0.0.1` | nothing is reachable from another host until an address says so |
| `ftp.port`, `ftps.port`, `sftp.port`, `http.port`, `https.port`, `tftp.port` | `8021`, `8990`, `8022`, `8080`, `8443`, `8069` | above 1024, so no privileges are needed |
| `tftp.allowWrite` | `false` | read only unless switched on |
| `tftp.maxBlockSize` | `1468` | keeps a block inside a typical ethernet MTU so datagrams are not IP fragmented |
| `tftp.maxTimeout` | `60` | a client cannot negotiate a retransmit interval that pins a transfer slot |
| `tftp.maxConnectionsPerHost` | `5` | one host cannot take every slot |
| `[[users]]` in the shipped file | `admin`, password `admin` | the one account that opens the admin interface after installing; its password is warned about until it is changed |

These defaults changed after 0.9.17: before, FTP and TFTP were on, on ports 21 and
69, every server listened on all interfaces, and HTTP was off. A configuration
that sets `enabled`, `port` and `address` in each section, as one written from
the old template does, is not affected. One that leaves them out now gets the
new values, so check it before updating.

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

`PROT P`, which asks for the data connection to be protected, is refused on a
plaintext control connection: answering it there would promise exactly what the
server then cannot deliver, and the client would send its data in the clear
believing otherwise.

`[ftps]` is a section of its own only because TOML tables are top level: it
configures the same server, which serves the folders, accounts and limits of
`[ftp]` on both listeners. The two `enabled` switches are independent, so
`ftp.enabled = false` with `ftps.enabled = true` serves implicit FTPS with
nothing on the plaintext port.

With `cert` and `key` empty the server generates a self-signed certificate at
the first start, keeps it in the persistent data file and says so. That
certificate proves no identity; it is there so the TLS interface works out of
the box for a test, not for production.
