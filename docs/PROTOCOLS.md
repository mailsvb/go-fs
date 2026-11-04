# Protocols

What each server speaks and how to set it up: FTP, SFTP, HTTP, WebDAV, S3 and TFTP.

Every server serves the same tree, and none of them shows or reaches a folder
`.go-fs` at its top, which go-fs keeps for the fixed content it serves over
HTTP, the document viewer for one; see [The folder `/.go-fs`](BROWSER.md#the-folder-go-fs).

## The FTP data channel

FTP carries its data on a second connection, and which end opens it is the
client's choice. Both are always served; what the configuration decides is the
ports each one uses, which is what a firewall in front of the server has to be
told.

```toml
[ftp]
# passive (PASV, EPSV): the client connects in, on a port out of this range
passiveMinPort = 1024
passiveMaxPort = 1034
# the address a PASV reply names, empty for the one the client connected to
passiveAddress = ""
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

**Behind NAT**, in a container or through a port forward, the address the server
sees on its own socket is not the one clients reach it at, and a `PASV` reply
that names it sends them nowhere. `passiveAddress` is what they are told
instead. It has to be IPv4, which is all a `PASV` reply can carry; `EPSV` names
no address at all and needs nothing here.

All of these apply to the next transfer, so changing them — in the file or in
the web interface — never drops a connection.

## SFTP

SFTP is the file transfer subsystem of SSH, so `[sftp]` runs an SSH server. It
serves only that subsystem: a `shell` or `exec` request is refused, and there is
no way to run anything on the host through it.

```toml
[[users]]
username = "john"
password = "doe"
sftp = true
allowUserFileRetrieve = true

[[users]]
username = "max"
sftp = true
authorizedKeys = ["ssh-ed25519 AAAAC3Nz... max@laptop"]
allowUserFileRetrieve = true
allowUserFileCreate = true

[sftp]
enabled = true
port = 2222
hostkey = ""
```

The accounts are the `[[users]]` entries that set `sftp = true`. An account
authenticates with a password, with a public key, or with either when both are
configured. `authorizedKeys` entries are `authorized_keys` lines, the content
of an `id_*.pub` file. `allowLoginWithoutPassword` means nothing here — SSH has
no anonymous login — so an account needs a password or a key, and one with
neither is refused at startup rather than left unusable.

The host key lives in the configuration itself rather than in a separate file,
as every certificate and key here does: `hostkey` is base64 of its PEM encoding,
on one line. With it empty a key is generated at every start, which makes every
client report a changed host key, so set it for anything but a first look —
with **Generate** in the web interface, or by hand:

```shell
ssh-keygen -q -t ed25519 -N "" -f hostkey && base64 < hostkey | tr -d "\n"
```

### SSH algorithms and limits

`[general.ssh]` holds the SSH transport, for the SFTP server and for sending a
file to another host from the listing alike. Its algorithm lists start empty,
which stands for every algorithm the SSH library implements and considers
secure: SHA-1 key exchange, `hmac-sha1-96`, `ssh-rsa` signatures and DSA are
not offered. A client that needs one of them is turned away with
`sftp client has no algorithm in common` at info level, naming what it
offered; listing the algorithm lets it in, and the server warns at startup
that it does.

```toml
[general.ssh]
keyExchanges = []          # empty: the secure default
ciphers = []
macs = []
publicKeyAlgorithms = []   # "ssh-rsa" here admits RSA keys signed with SHA-1
hostKeyAlgorithms = []
loginGraceTime = 30        # seconds to finish the handshake and log in
maxAuthTries = 6
maxSessions = 10           # per connection
maxConnectionsPerHost = 0  # per address, 0 for no limit beside maxConnections
keepAliveInterval = 60     # seconds, 0 disables
keepAliveCountMax = 3
maxPacketSize = 32768      # largest answer to one read, up to 261120
banner = []                # lines shown before the login
```

| Setting | Why |
|---|---|
| `loginGraceTime` | a client that connects and never logs in gives its slot back, instead of holding it until `sftp.idleTimeout` |
| `keepAliveInterval`, `keepAliveCountMax` | a client that went away without closing is found; answered keepalives do not count as activity, so `sftp.idleTimeout` still closes an idle session |
| `maxConnectionsPerHost`, `maxSessions` | one address, or one connection, cannot take every slot |
| `maxPacketSize` | a client set to read more at a time (`sftp -B`, `rclone --sftp-chunk-size`) is answered in fewer round trips |

On the server the client's order of preference picks among what both sides
offer, so a list there only narrows the choice. Sending a file, go-fs is the
client and its order counts; with `ciphers` empty, chacha20-poly1305 comes
first on a processor without AES instructions, such as a Raspberry Pi 4, where
it is several times faster than AES-GCM.

The cipher matters less than the round trip: the SSH library keeps the window
of a channel at 2 MB, so one transfer moves at most 2 MB per round trip, about
40 MB/s at 50 ms of latency.

## HTTP

`[http]` serves the folder over HTTP: `GET` browses and downloads, `PUT`
uploads, `DELETE` removes, `MKCOL` creates a folder and `MOVE` renames one entry
in place. `[https]` is the same server on a TLS port, with `cert` and `key`
holding the material as in `[ftps]`, and the two `enabled` switches are
independent. With `webdav = true` the same folder also mounts as a drive in
Explorer and Finder; see [Mounting as a drive](#mounting-as-a-drive-webdav).

Browsing it in a browser gives a listing that sorts on any column, filters as you
type, and — for an account that holds the rights — uploads by drag and drop,
creates folders, renames and deletes. Each row has an options menu with
**Download** — a folder downloads as one `.tar.xz` — and, where the account may,
**Rename** and **Delete**. It needs no assets from anywhere: the page
carries its own style and script, so every URL the server answers stays a path in
`general.baseFolderFiles`. Without JavaScript the listing still renders and the column headers
still sort, as ordinary links.

Access has two layers:

```toml
[[users]]
username = "john"
password = "doe"
http = true
paths = ["^/private/.*"]
allowUserFileRetrieve = true
allowUserFileCreate = true
allowUserFileOverwrite = true
allowUserFolderCreate = true
allowUserFileDelete = true
allowUserFolderDelete = true

[http]
enabled = true
port = 8080
pathsRequireAuth = ["^/private/.*"]
httpSessionTokenLifetime = 28800   # idle timeout, renewed while in use
httpSessionMaxLifetime = 604800    # a login lasts a week at most
httpSessionTokenSecret = ""        # generated at the first start
shareLinkSecret = ""               # generated at the first start, see "Sharing a file"
loginAttempts = 5
loginLockout = 60
trustedProxies = []
```

**Reading is public** — `GET` and `HEAD` — unless the path matches one of
`pathsRequireAuth`; `pathsRequireAuth = ["^/"]` makes the whole server
private. **Everything else always needs an account**: every method that changes
something (`PUT`, `DELETE`, `POST`, `MKCOL`, `MOVE`, and WebDAV's `COPY`,
`LOCK`, `UNLOCK` and `PROPPATCH`) and WebDAV's listing, `PROPFIND`. There is no
public upload. Such a request has to be answered by one of
the `[[users]]` entries that set `http = true`, and that account's own `paths`
then decide what it may reach, and its rights what it may do there: the same
`allowUser*` flags as on the other servers, mapped as the table under Accounts
says — `allowUserFileCreate` for a `PUT` of a new name, `allowUserFileOverwrite`
for a `PUT` over a file that exists, `allowUserFolderCreate` for `MKCOL`,
`allowUserFileDelete` and `allowUserFolderDelete` for `DELETE` of the one or
the other, create and delete both for `MOVE`, and `allowUserFileRetrieve` for
reading and listing, all false unless set. A path an account may not reach is
`403`, not another challenge.

`methodsRequireAuth` is gone: a file that still sets it loads, and the key is
ignored with a warning at startup. A file that listed `GET` there, to keep the
whole server private, needs `pathsRequireAuth = ["^/"]` instead.

`paths` are matched against the request path **after** it has been normalized,
so `/private/../secret` is tested as `/secret` and cannot be used to slip past a
pattern. A pattern is tested against the path both with and without a trailing
slash, so `^/private/.*` covers the listing of `/private` itself and not only
what is inside it — a listing names every file in the folder, so it cannot be
the one public thing about it.

Both Digest and Basic authentication are accepted. A **program** is challenged
with Digest, using SHA-256 for Chromium and Firefox and MD5 for everything
else, which is what those clients handle. A digest response is bound to the path it was made
for and to a nonce that is good for five minutes, so a header captured off the
wire cannot be turned on another path or replayed later; a client that still has
the credentials answers the stale challenge without asking anyone. `curl -u`
and scripts are unaffected by everything below.

**Wrong passwords are counted per client address.** The login form, Basic and
Digest share one count: an address that sends `loginAttempts` of them inside
`loginLockout` seconds is refused for that long, at once and without a look at
what it sent. A program is answered `429` with `Retry-After`, and no challenge,
since a challenge asks for the very thing the lock refuses to read; a browser
is sent to the login page, which says so. A stale digest nonce is not a wrong
password and is not counted, a right password clears the count, and a session
token is never refused, so someone logged in behind the same address as a
guesser keeps their login. `loginFailureDelay` still holds every counted
failure for a second; the lock is what bounds how many of those one address
can cause. `loginAttempts = 0` switches the lock off.

The credential check takes the same time whichever name is sent: the name and
the password are compared for every account, so an account that exists is
refused no faster than one that does not.

### Mounting as a drive (WebDAV)

With `webdav = true` the HTTP server also answers WebDAV (RFC 4918, class 1
and 2) on the paths it already serves, on both listeners, so the folder mounts
as a drive with nothing to install:

- **macOS Finder:** Go → Connect to Server (⌘K), `https://host:8443/`
- **Windows Explorer:** This PC → Map network drive, `https://host:8443/`, or
  `net use Z: https://host:8443/`
- **Linux:** `davs://host:8443/` in GNOME Files, or davfs2

The drive is always mounted with an account. These clients send their
credentials only when the server asks for them, so every `PROPFIND` needs an
account, even of a folder anyone may read with `GET`. Otherwise the drive
would show what a visitor without an account sees.

go-fs-light is built without WebDAV, and ignores `webdav = true`; so does a
build with the tag `no_webdav`.

```toml
[http]
webdav = true
webdavClients = ["Microsoft-WebDAV-MiniRedir", "WebDAVFS", "WebDAVLib", "Microsoft Office",
  "davfs2", "gvfs", "Cyberduck", "Mountain Duck", "rclone", "WinSCP"]

[https]
enabled = true
```

WebDAV shares `PUT`, `DELETE` and `MOVE` with the browser page and with
scripts, and gives two of them another meaning:

- **`DELETE` of a folder** removes it with everything in it.
- **`MOVE`** moves across folders, and onto a name that is taken when the
  client sends `Overwrite: T`, which is how many programs save a file.

**Only a WebDAV client gets that meaning.** A client is taken for one when its
`User-Agent` contains one of `webdavClients`, ignoring case, or when the
request carries a `Depth` or an `If` header, which only WebDAV defines.
Everyone else keeps the meaning these methods always had: the page and a
`curl -X DELETE` still cannot remove a folder that is not empty. `PROPFIND`,
`PROPPATCH`, `LOCK`, `UNLOCK`, `COPY` and `OPTIONS` are WebDAV whoever sends
them. `GET`, `HEAD` and `MKCOL` mean the same to everyone and are answered as
they always were.

**Telling the clients apart decides what a method means, never what an account
may do.** Every path a request touches is checked against the account's
`paths` and rights, as the page's requests are:

| Request | Needs |
|---|---|
| `PROPFIND` (listing) | `allowUserFileRetrieve`; a listing leaves out what the account may not reach |
| `PUT`, `LOCK` of a free name | `allowUserFileCreate` |
| `PUT`, `LOCK` of a taken name | `allowUserFileOverwrite`, except for an empty file (see below) |
| `DELETE` of a folder | `allowUserFileDelete` and `allowUserFolderDelete` for everything in it, at paths the account may reach; if one thing fails, nothing is removed |
| `MOVE` | create and delete, at the path each file leaves and the path it lands on; overwriting also needs the right to delete what is there |
| `COPY` | `allowUserFileRetrieve` on what is copied and `allowUserFileCreate`/`allowUserFolderCreate` for the copies; overwriting also needs the right to delete what is there |
| `PROPPATCH`, `UNLOCK` | `allowUserFileRetrieve`; properties are accepted but not stored |

Explorer and Finder create a file with an empty `PUT` and lock it before
sending what is in it. So an empty file counts as a free name, and
`allowUserFileCreate` alone is enough to fill it. A file with anything in it
still takes `allowUserFileOverwrite`.

A `MOVE` or `COPY` onto its own source, or into or onto a folder that holds
it, is refused, because overwriting the destination would remove the source
with it. The one exception is renaming to another spelling of the same name
on a filesystem that ignores case, which simply renames.

**Locks** are kept in memory, so a restart drops them. While a WebDAV client
holds one, a `PUT`, `DELETE`, `MKCOL` or `MOVE` of that path from the page or
a script is answered `423 Locked`.

**Limits:**

- A `PROPFIND` has to say `Depth: 0` or `Depth: 1`. Walking the whole tree in
  one request is refused, as RFC 4918 allows.
- `maxUploadSize` and `readTimeout` hold for WebDAV uploads as they do for the
  page's.
- An upload is written to `general.baseFolderTemp` and moved to its name once it
  has arrived whole, so a copy that breaks off leaves nothing behind.

**Windows** sends a password to a WebDAV server only over TLS, and only when
it trusts the certificate. A self-signed certificate therefore has to be
imported into the Trusted Root Certification Authorities store of the machine
first, or replaced by one Windows already trusts. Over TLS the challenge offers
Basic beside Digest, since Explorer answers nothing else; over the plain port
it offers Digest alone. Explorer mounts through the WebClient service, so if
mapping the drive fails at once, check that the service is running
(`sc query webclient`). By default Windows also refuses to download a file
larger than 50 MB over WebDAV. The limit is `FileSizeLimitInBytes` under
`HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters`; raise it and
restart the service.

**Logging in:** a client is asked for an account only when a request needs
one. On a server whose `GET` is public, a drive therefore mounts without a
login and is read-only until something asks. Protect `GET`, or the paths, to
be asked at once. A client that retries a wrong password by itself can use
up `loginAttempts` with one typo.

Finder writes `._name` and `.DS_Store` files beside what it copies, as it does
on every network drive.

### Bearer tokens

A script or an API client can use a **bearer token** instead of an account.
Tokens are listed under `[[tokens]]` and managed on the **TOKENS** tab of the
admin interface, next to USERS:

1. Give the token a name and a lifetime in days; `0` means it never expires.
2. The page shows the new token once. Copy it before pressing **Apply**, which
   writes the entry. The file holds only the token's SHA-256 hash, so a lost
   token has to be replaced; it cannot be recovered.

Clients send the token as a header, on the plain port and the TLS port alike:

```sh
curl -H "Authorization: Bearer gofs_…" https://host:8443/reports/today.csv
curl -H "Authorization: Bearer gofs_…" -T build.zip https://host:8443/uploads/build.zip
```

What a token may do is set on its own entry:

- **Rights:** the same `allowUser*` rights as an account.
- **`paths`:** regular expressions matched against the request path. A token
  with no pattern can reach nothing.
- **`expires`:** an RFC 3339 time. Edit it to extend a token, or clear it to
  make the token permanent.

Only the HTTP server accepts tokens. FTP, SFTP and TFTP never see them.

A token is not a session, so it cannot open the admin interface. It also
counts in the login lock: an unknown token is counted as a wrong password.

An unknown or expired token is answered `401` with
`WWW-Authenticate: Bearer error="invalid_token"`, even on a public path, so the
client learns that its token is not accepted.

Removing a token from the file revokes it within one reload.

### S3

The HTTP server also answers the **S3 API**, on its plain port and its TLS port
alike, so that S3 clients such as the aws CLI, rclone and the AWS SDKs can use
the served folder. It is on by default with
`http.enableS3 = true`; each account that should use it sets `s3 = true`:

```toml
[[users]]
username = "backup"      # the access key
password = "…"           # the secret key
s3 = true
paths = ["^/backups/.*"]
allowUserFileRetrieve = true
allowUserFileCreate = true
allowUserFolderCreate = true
```

| | |
|---|---|
| Endpoint | `https://host:8443` (or the plain port) |
| Buckets | every folder directly in `general.baseFolderFiles`, by its name ignoring case |
| Region | `us-east-1`, the one region, fixed |
| Access key / secret key | the account's `username` / `password` |
| Addressing | path style only: `https://host:8443/<folder>/<key>` |

Each folder directly in `general.baseFolderFiles` is a bucket holding that folder exactly
as HTTP shows it: `s3://docs/a.txt` is the file served at `/docs/a.txt`, and the
account's `paths` are matched against that same path. A file directly in
`general.baseFolderFiles` is in no bucket and cannot be reached over S3. A bucket name is
matched against the folder names ignoring case, so `s3://docs/` reaches the
folder `Docs`, and the folder is named as it is spelled on disk in every answer.
Where two folders differ in case only, the one spelled as asked wins, and
otherwise the first by name. `ListBuckets` lists the folders the account's
`paths` reach. Its rights mean what they mean everywhere else, so an account has
the same reach over S3 as over HTTP. The region is shown read-only on the HTTP
tab of the admin interface and cannot be changed. Buckets cannot be created or
removed over S3: make or remove the folder instead. `s3` is independent of `http`: an
account that sets only `s3` cannot log in to the page or send Basic credentials.

Only requests signed with AWS Signature Version 4 are S3 requests, in the
`Authorization` header or as a presigned URL. Everything else is served as it
always was, so a bucket's folder is still reached as `/docs` over HTTP, and
nothing is public over S3. A request signed for another region is refused
with the answer AWS gives, which tells the SDKs to sign for `us-east-1`.
Signatures are checked against the clock with 15 minutes of slack, and a wrong
secret key counts against the address in the same lock as a wrong password.

Clients have to be set to path style and to the region. For example:

```shell
aws configure set default.s3.addressing_style path
aws --endpoint-url https://host:8443 --region us-east-1 s3 ls
aws --endpoint-url https://host:8443 --region us-east-1 s3 ls s3://backups/
aws --endpoint-url https://host:8443 --region us-east-1 s3 cp report.pdf s3://backups/2026/
```

Version 1 of the aws CLI presigns URLs with Signature Version 2 unless told
otherwise, so `aws s3 presign` needs
`aws configure set default.s3.signature_version s3v4` there; version 2 of the
CLI signs with version 4 already.

```ini
# rclone.conf
[go-fs]
type = s3
provider = Other
endpoint = https://host:8443
region = us-east-1
force_path_style = true
access_key_id = backup
secret_access_key = …
```

What is served:

- **Reading.** `ListBuckets`, `HeadBucket`, `GetBucketLocation`,
  `ListObjects` and `ListObjectsV2` (with `prefix`, `delimiter`, paging and
  `encoding-type=url`), `GetObject` with ranges and conditions, `HeadObject`,
  and presigned downloads.
- **Folders.** A folder is a common prefix in a listing. `PutObject` of an empty
  key ending in `/` creates one, as every S3 client does. A folder with nothing
  in it is listed as that `folder/` key.
- **Uploading.** `PutObject`, and multipart upload with `UploadPart`,
  `UploadPartCopy`, `ListParts`, `ListMultipartUploads`, `Complete…` and
  `Abort…`. The body may be signed, unsigned or `aws-chunked` with or without
  chunk signatures. `Content-MD5` and the `x-amz-checksum-*` of CRC32, CRC32C,
  CRC64NVME, SHA-1 and SHA-256 are verified, whether they come as a header or
  as a trailer. An upload is staged in `general.baseFolderTemp` and only
  moved into place once every hash matches; `maxUploadSize` applies.
  Unfinished multipart uploads are swept after 24 hours.
- **Renaming.** S3 has no rename. Clients rename with `CopyObject` (or
  `UploadPartCopy` for a large file) and a `DeleteObject`, and rename a folder
  one key at a time. Here too the data is copied, so renaming a large folder
  takes as long as copying it, and it needs `allowUserFileRetrieve`, create and
  delete. An account without the delete right is left with the copy.
- **Deleting.** `DeleteObject` and `DeleteObjects`. A folder is removed only
  when it is empty. When a delete leaves the folders above it empty, they are
  removed as well for an account with `allowUserFolderDelete`: that is how a
  folder behaves in S3, and it is what makes a renamed folder disappear from
  its old place.

What is not served: virtual-hosted addressing (`main.host`), versioning, ACLs,
policies, tagging and object lock, which are answered `NotImplemented`; user
metadata, which is accepted and not stored; and Signature Version 2, which is
not taken for an S3 request at all.

- **ETags.** A listing, `HeadObject` and `GetObject` give each file an ETag
  derived from its size and modification time, in the multipart form
  `"…-1"`, so that clients do not take it for an MD5. The ETag of an upload is
  the MD5 of what arrived, and the ETag of a completed multipart upload is
  S3's own form for one.
- **Listing without a delimiter.** This reads every folder below the prefix
  and sorts the result, so on a very large tree it is slow.
- **The password is the secret key.** Changing it breaks every client
  configured with the old one. SigV4 never sends the key, but a request
  captured over plain HTTP allows a password to be guessed offline, as a Digest
  response does, so give S3 accounts long random passwords and use the TLS
  port.

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
`application/octet-stream` (or no `Content-Type`, as `curl -T` sends) and multipart uploads, `DELETE` for a file or an
empty folder, Basic (RFC 7617) and Digest (RFC 7616, with the RFC 2069 form)
authentication, bearer tokens (RFC 6750) for scripts, browser login with a signed session token (JWT, RFC 7519), and
the `dls_directory_reader` listing endpoint. Downloads answer range requests, so a large one can be resumed.
The OSEM directory scanner (the HTTPS storage provider) posts to `dls_directory_reader.php` and `.asp` with its fixed
credentials in the form (`PHP_DLS_USER`, `PHP_DLS_PW`) instead of an `Authorization` header, as the original
scripts expected. A POST that carries them needs what browsing the listed folder with `GET` needs, though every
other POST needs an account. The pair is public, so it opens no protected folder. A folder in
`pathsRequireAuth` still needs Basic, Digest or bearer credentials as well.

**WebDAV** — RFC 4918 class 1 and 2 on the HTTP listeners, through
`golang.org/x/net/webdav`, when `http.webdav` is on: `OPTIONS`, `PROPFIND` of
depth 0 and 1, `PROPPATCH` (accepted, not stored), `PUT`, recursive `DELETE`,
`COPY`, `MOVE`, and exclusive and shared write locks with `LOCK` and `UNLOCK`,
kept in memory. `GET`, `HEAD` and `MKCOL` are the HTTP server's own.

**S3** — the object API of Amazon S3 on the HTTP listeners, authenticated with
AWS Signature Version 4 (header, presigned URL, signed and unsigned
`aws-chunked` bodies with trailers), path style, with every top-level folder a bucket, in one region.
Bucket: `ListBuckets`, `HeadBucket`, `GetBucketLocation`, `GetBucketVersioning`,
`ListObjects`, `ListObjectsV2`, `ListMultipartUploads`, `DeleteObjects`, and
`CreateBucket` of a bucket whose folder exists. Object: `GetObject`, `HeadObject`,
`PutObject`, `CopyObject`, `DeleteObject`, `CreateMultipartUpload`,
`UploadPart`, `UploadPartCopy`, `ListParts`, `CompleteMultipartUpload` and
`AbortMultipartUpload`.

**Registry** — the OCI Distribution Specification v1.1 under `/v2/` on the HTTP
listeners, with the Bearer token flow of the Docker registry (a JWT, RFC 7519,
from `/v2/_token`) and HTTP Basic authentication: pulling manifests and blobs,
monolithic, chunked, streamed and mounted blob uploads, pushing and deleting
manifests and tags, the tag list, the `_catalog` extension and the referrers
API. OCI image manifests and indexes and Docker schema 2 manifests and
manifest lists; SHA-256 and SHA-512 digests. Images for different platforms
pushed to one tag are merged into an index.

**TFTP** — the protocol has no accounts and no passwords and no way to carry
them, so anyone who can reach `tftp.port` can read what `allowRead` allows and
write what `allowWrite` allows. It is off by default, and read only when turned
on; keep it on a network where that is what you want. An upload is written beside its
destination and renamed over it when it completes, so a transfer that breaks off
leaves neither an empty file nor a truncated one.

RFC 1350 in `octet` and `netascii` mode, with the option extension of
RFC 2347, the `blksize`, `timeout` and `tsize` options of RFC 2348 and RFC 2349,
and windowed reads per RFC 7440.
