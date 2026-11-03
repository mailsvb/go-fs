# Remote servers

Sending files to other hosts, browsing them, keeping files there encrypted, and syncing folders with them.

## Sending a file to another server

**Send File…** in the row menu of a file uploads it to another host, over SFTP
or SMB (see [SMB](#smb)), or to a JFrog Artifactory (see
[Artifactory](#artifactory)). It is offered to
an account that has logged in through the form and may read the file. The
dialog opens with a **Server** list: the servers an admin stored (see
[Stored servers](#stored-servers)), and **Manual**, chosen at first, which
asks for a login instead.

A stored server is listed only when it allows uploads (`allowUpload`, see
[Stored servers](#stored-servers)) and the account may use it
(`allowedUsers`). It is picked by its name. **Connect** goes straight to its
folders with the login and the host key the configuration file holds. The
page never sees the password or the key; a host that shows another key than
the stored one is not logged in to, and the dialog says that an administrator
has to edit the server.

**Manual** takes three steps, and stores nothing:

1. **Connect.** The protocol (SFTP), the host name or address, the port (22
   unless another is given), a username and a password. The password is sent
   as SSH `password` authentication, and as the answer to every prompt where
   the host asks by `keyboard-interactive` instead. For **SMB** the port is
   445 unless another is given, and the dialog goes straight to the folders,
   since an SMB host shows no key. For **Artifactory** the dialog asks for the
   server address and a token instead, and goes straight to the folders too.
2. **Host key.** Before any login is offered, the server connects to the host
   only far enough to see its key, and the dialog shows the key type and its
   SHA256 fingerprint, as `ssh` does. **Accept key** goes on; every later
   request names the fingerprint that was accepted, and a host that shows
   another one is not logged in to. The browser remembers the accepted key
   for that host and port, in its local storage, so the next send there skips
   this step. A key that changed since is shown again, with a warning, and has
   to be accepted anew. Nothing about keys is stored on the server.
3. **Folder.** The server logs in and lists the folder the login starts in.
   A click on a folder, on `..` or on a part of the path above the list goes
   there. Files are shown, but only folders can be chosen. A file of the same
   name is marked, and the line below the list then says it will be replaced.
   **Send** starts the upload into the folder shown.

The server sends the file in the background, as it fetches one: once the
upload has begun the dialog closes and the send is listed under **Transfers**
with the fetches, with the same progress, speed and **Stop**. The file is
written under a hidden name, `.<name>.go-fs-<id>.part`, in the chosen folder
and renamed into place once all of it is there, with
`posix-rename@openssh.com` where an SFTP host offers it, so that a send that fails
or is stopped never leaves half a file under the real name. A stopped send
takes its partial file away again. An Artifactory needs none of that: it keeps
an upload only once all of it was taken. A send that moves nothing for two minutes
stops with an error. The dialog keeps the host and the login until the page is
reloaded, so the next file opens on the folder the last one went to; the
password is never stored, and it is never logged.

The endpoints are on the file, all `POST`:

- `?go-fs=send-hostkey` takes `host` and `port`, and optionally `protocol`
  (`sftp`), and answers `host`, `keyType` and `fingerprint`.
- `?go-fs=send-browse` takes `host`, `port`, `username`, `password`, `hostKey`
  (the accepted fingerprint) and `path` (empty for the folder the login starts
  in), or `protocol` `artifactory` with `url`, `token` and `path`, or `server`
  (the name of a stored server) and `path`. It answers `base` (`sftp://host`,
  `smb://host`, or the address of an Artifactory), `path`, `parent` and `entries` (`name`,
  `dir`, `size`).
- `?go-fs=send` takes the same fields, with `path` as the folder to send into,
  and answers `202` with the job.

A body that names a `server` is sent with that server's login; whatever else
it names is ignored. `send-hostkey` refuses a stored server, whose key was
accepted when it was stored.

A send's progress is read and stopped through `?go-fs=fetch-job`, and it is
listed by `?go-fs=fetch-jobs` with `kind` `send`. A remote that cannot be
reached, refuses the login or has no such folder is answered `502` with the
reason. As with Fetch, the server connects from its own network to whatever
host is typed in, internal addresses included.

### Stored servers

The servers the dialog lists are `[[servers]]` entries, managed on the
**SERVERS** tab of the admin interface:

1. **Add server…** asks for a name, the protocol, the host, the port, a
   username and a password; for Artifactory, for a name, the server address
   and a token instead. For SMB and Artifactory it skips to the last step,
   since neither shows a key.
2. **Connect** shows the key the host shows, without logging in, as the send
   dialog does. When a server is edited, the dialog says whether the key is
   the one the server is stored with.
3. **Accept key and save** logs in with the login and that key. Only once that
   worked is the entry written to the file, at once, with nothing else on the
   page; other changes still wait for **Apply**. The file is rewritten and kept
   as `.bak`, as Apply does.

On the tab, the name, the rights and the allowed users of a server can be changed and the server
removed, all written by **Apply**; everything else is read-only and changed
with **Edit…**, which goes through the same dialog. An edit that leaves the
password or the token empty keeps the stored one, and an edit keeps the
rights and the allowed users as they are.

```toml
[[servers]]
name = "backup host"
type = "sftp"
host = "sftp.example.com"
port = 22
username = "backup"
password = "secret"
hostKeyFingerprint = "SHA256:…"
allowedUsers = ["alice", "bob"]
```

The name has to be unique, whatever its case, and cannot be `Manual`. The
password is kept in plain text, as an account's is.

`allowedUsers` names the accounts, one per line on the admin tab, that may use
the server: see it in the menu, browse it, download and fetch from it, and send
to it. It is empty when not set. An account that sets `isAdmin` sees and may
use every server, named or not; any other account that is not named has no
access to the server at all, and is answered as if it did not exist. Names are
matched with their case, as logging in matches them.

What go-fs may do on a server is set per server, for every account, on top of
what the account itself may: an action needs both.

| key | allows | when not set |
|---|---|---|
| `allowDownload` | downloading a file, a folder as `.tar.xz`, and **Fetch…** into the served tree | `true` |
| `allowUpload` | uploading to the server, and **Send File…** to it | `false` |
| `allowCreate` | **New folder** | `false` |
| `allowDelete` | deleting a file or a folder | `false` |
| `allowRename` | renaming a file or a folder | `false` |

A server stored without any of them can be browsed and downloaded from, and
nothing more. Its folders are always listed; one without `allowDownload` shows
its files without a link or **Download**.

### Encrypted servers

A server can keep the files go-fs puts on it in a vault of
[Cryptomator](https://cryptomator.org) (vault format 8, as the current apps
make it), so that the server and whoever runs it only ever see ciphertext:
the names of the files and the folders and what they hold are encrypted by
go-fs before they leave it and decrypted when they come back. Browsing,
downloading, **Fetch…**, uploading and **Send File…** all work as on any server.

```toml
[[servers]]
name = "backup host"
type = "sftp"
host = "sftp.example.com"
username = "backup"
password = "secret"
hostKeyFingerprint = "SHA256:…"
vaultPath = "vaults/backup"
vaultPassword = "another secret"
```

- `vaultPath` is the folder of the vault on the server. A path without a slash
  in front is from where the login starts, the home folder for SFTP; on SMB and
  Artifactory it starts with the share or the repository. With a vault, go-fs
  sees the vault and nothing beside it: to reach the rest of the host too,
  store it a second time without one.
- In the **Edit…** dialog, **new vault** makes a new vault in the folder, which
  has to be empty or not there yet; without it, the dialog opens the vault that
  is there, and refuses to save the server when the password does not unlock
  it. An edit that leaves the vault password empty keeps the stored one.
- A new vault's **recovery key** is shown once, on the server's card, until the
  page is read again. It is the key of the vault itself: it opens the vault
  whatever its password, and go-fs keeps it nowhere. It is not the word list the
  Cryptomator apps show, so their recovery dialog does not take it.
- The Cryptomator apps open a vault go-fs made, and go-fs opens one they made
  with a password. Vaults unlocked by Cryptomator Hub and the older
  `SIV_CTRMAC` vaults are refused. Links inside a vault are not followed.
- go-fs-light is built without vaults. It leaves a server with a `vaultPath`
  alone, as it does one of a protocol it does not have: the server is kept in
  the file, named at startup as ignored, and neither offered nor reached, so
  that nothing ever lands beside the vault in cleartext.
- `vaultPassword` is kept in plain text, as the password is, so a vault keeps
  its files from the server, not from whoever can read the configuration.
  A file sent with the **Manual** login is never encrypted.

### Artifactory

A JFrog Artifactory is a server of `type = "artifactory"`, reached by its REST
API over HTTPS (or HTTP) with a token, sent as `Authorization: Bearer <token>`:
an access token or an identity token of a user, made in Artifactory under
**Edit Profile** or **Access Tokens**. It is stored with its address and the
token, in plain text as a password is:

```toml
[[servers]]
name = "artifacts"
type = "artifactory"
url = "https://acme.jfrog.io/artifactory"
token = "…"
```

The address is where the REST API is. One without a path, such as
`https://acme.jfrog.io`, is given `/artifactory`, where every Artifactory has
it; one with a path, for an Artifactory behind a proxy that moved it, is kept
as it is. The admin dialog checks the token by listing the repositories
(`GET api/repositories`), which an Artifactory refuses a wrong token for.

Its files are in repositories, so the top of the server, `/`, lists the
repositories the token may see as folders, and `/<repository>/<path>` is a
file or a folder in one. Files and folders go into a repository; a
repository itself is created, renamed and removed in Artifactory. What else
the token may do, deploying, replacing or deleting, is what Artifactory
grants it.

A folder is listed with the sizes and times of its files by the File List
API, which is of Artifactory Pro and the JFrog cloud. An Artifactory without
it (the OSS edition) is listed by the children of the folder, which have no
size or time. A rename is the Move API, also of Pro; without it a rename fails
with what Artifactory says. A folder is deleted only when there is nothing in
it, as on an SFTP server, although Artifactory would remove all of it.

### SMB

A Windows file server, a NAS or a Samba is a server of `type = "smb"`,
reached by SMB 2 or 3 on port 445 and logged in to by NTLMv2 with a username
and a password. A username of `DOMAIN\user` logs in to that domain; a plain
one, or `user@domain`, is handed on as it is. SMB shows no host key, so there
is nothing to accept: the admin dialog stores the server once the login
works.

```toml
[[servers]]
name = "file server"
type = "smb"
host = "files.example.com"
username = "CORP\\backup"
password = "secret"
```

Its files are in shares, so the top of the server, `/`, lists the shares the
login may see as folders, and `/<share>/<path>` is a file or a folder in one.
The shares whose names end in `$` (`C$`, `ADMIN$`, `IPC$`) are left out of
the list; a host that does not list its shares to the login is still reached
by the path of a share. Files and folders go into a share; a share itself is
created, renamed and removed on the host. A rename stays within its share. A
send replaces a file of the same name, as on an SFTP server, but never a
folder.

SMB 1 is not spoken, nor Kerberos: a host that only takes those refuses the
login.

### Browsing a stored server

The menu in the corner of the header lists the stored servers by name, after
**Files** and **Registry** and before **Admin** and **Log out**. It lists them
to an account that has logged in through the form and may read files
(`allowUserFileRetrieve`), the same accounts that may send.

Picking a server logs in with its stored login and key, and shows the folder
the login starts in; for an Artifactory, the list of its repositories, and
for an SMB host, the list of its shares. The page is the file listing: crumbs, sortable columns,
the filter, **Upload or Drop file**, **New folder**, the **Transfers** button,
and the row menu with **Download**, **Rename…** and **Delete**. Download on a
folder packs it as a `.tar.xz`, as it does for a folder of the served tree.
**Share** and **Send File** are left out, because they hand out files that
go-fs serves itself. Fetch and Import into registry are left out as well.

Everything goes through go-fs, which logs in anew for every request, so the
page never sees the password or the key. A server that cannot be reached,
refuses the login or shows a different key is shown as an empty listing, with
the reason in the banner.

What an account may change on the server follows the rights of the server
(see [Stored servers](#stored-servers)) and its own permissions, as in the
served folder: `allowUserFileCreate` to upload, `allowUserFileOverwrite` to
replace, `allowUserFolderCreate` for New folder, `allowUserFileCreate` and
`allowUserFileDelete` to rename, and `allowUserFileDelete` and
`allowUserFolderDelete` to delete. The account's `paths` apply to the served
folder only. On the server, the stored login decides what can be reached.

An upload is written under a hidden name, `.<name>.go-fs-<id>.part`, and
renamed into place once all of it is there, as a send is; to an Artifactory
it goes straight to its name, which Artifactory keeps once all of it is
there. A chunked upload (`maxChunkSize`) is staged in `general.baseFolderTemp`
and goes to the server with its last chunk.

**Fetch…** in the menu of a file or a folder copies it into a folder of the
served tree, chosen in a dialog that lists that tree from its root. It is
offered where the server has `allowDownload` and the account has
`allowUserFileCreate`, and on a folder only with `allowUserFolderCreate` too.
The dialog shows how large what it fetches is: a file at once, and a folder
once go-fs has walked it, with the number of files and subfolders and their
total size. Folders below it that cannot be read are counted and left out.
It shows how much room the disk of the chosen folder has, and offers Fetch
only where everything fits. The fetch runs in the background, like a fetch
from a URL, and the transfers in the header follow it, a folder's with how
many of its files are done.

A folder lands as a folder of its name, with every subfolder, empty ones
included. If that folder is already there, the fetch fills it in. Before it
reads a byte, the server walks the folder again and refuses the whole fetch
if any of these fails:

- a name cannot be stored here;
- a folder may not be created;
- a file is there already and the account may not replace it;
- a file is larger than `maxUploadSize`;
- the files together are more than the disk of the folder has free;
- the largest file is more than the disk of the upload staging folder has free.

A replaced file is not counted as room, so a fetch over an earlier copy needs
as much free space as the first one did. A single file gets the same space
check, and so does a fetch from a URL whose answer says how large it is. A
folder fetch that fails or is stopped partway keeps the files it already
stored.

All requests go to the root, with the server and the path in the query:

```
/?go-fs=remote&server=<name>&path=<path on the server>
```

`GET` lists a folder or downloads a file, with ranges, and `GET` with
`&archive=1` packs a folder. `PUT` uploads (`application/octet-stream`, whole
or in `Content-Range` chunks), `MKCOL` creates a folder, `MOVE` renames to the
`path` of the `Destination` link on the same server, and `DELETE` removes a
file or an empty folder. Each one is logged as `http remote …` with the
account, the server and the path.

Fetch has its own markers on the root. `GET /?go-fs=local-browse&path=<folder>`
lists a folder of the served tree, with `writable`, `mkdir` and `free` (bytes,
`-1` when unknown). `GET ?go-fs=remote-measure&server=…&path=…` returns the
`files`, `folders`, `skipped`, `bytes` and `largest` of a file or folder of
the server. `POST ?go-fs=remote-fetch&server=…&path=…` with `{"folder": …}`
starts the fetch, which is followed through `?go-fs=fetch-job`. While a folder
fetch walks the folder, its phase is `listing`. Once it downloads, `blobsTotal`
and `blobsDone` count its files.

## Synchronising folders

A sync keeps a folder of `general.baseFolderFiles` and a folder of a
[stored server](#stored-servers) alike, on a schedule. Syncs are added on the
**SYNC** tab of the admin page, one dialog step at a time:

1. the **protocol**, and a name for the sync;
2. the stored **server** the protocol reaches;
3. the **remote** and the **local folder**, both picked by browsing them, the
   **direction** and whether to **delete**;
4. when it **runs**: every so many minutes or hours, or at a time of day on
   chosen days of the week.

go-fs logs in to the server and looks the remote folder up before it writes
the sync into the file. The tab shows how the last run went, when the next one
is and which paths failed; **Run now** starts one at once, and **enabled**
pauses a sync without removing it.

The one protocol is **rsync**. It is done by go-fs rather than by the rsync
program, so it needs nothing on the server but SFTP, works with the password
login a stored server has and inside an [encrypted server](#encrypted-servers),
and runs on every system go-fs does. Like rsync without its delta, it lists
both folders, copies whole every file whose size or modification time
differs, and gives the copy the modification time of the original, which is
how the next run knows it is alike. SFTP carries whole seconds, so times
within a second are the same. A copy is written under a hidden name and
renamed into place, on either side. Links are left alone, and so is a path
that is a file on one side and a folder on the other.

The direction is one of:

- **push**, from the local folder to the remote one;
- **pull**, from the remote folder to the local one;
- **both**: a file changed on one side since the last run is copied to the
  other; of one changed on both, the newer wins, the local one when they are
  as new, and the conflict is logged.

With **delete**, one way, what the target has and the source does not is
removed from the target. Both ways, a file removed on one side since the last
run is removed on the other, unless it was changed there meanwhile; without
delete, it is copied back. To tell a removed file from a new one, a sync both
ways keeps what it had in sync at the end of each run in the
[persistent data file](INSTALL.md#run); a `go-fs-sync` folder an earlier version kept
beside the configuration file is moved into it at the next start. Its first
run, or the first after its folders or its server changed, removes nothing. What a sync removes from
the local folder goes to [the trash](BROWSER.md#the-trash) when there is one, and every
removal is logged as `sync removed`. As a guard against a disk that is not
mounted or a folder emptied by mistake, nothing is removed while one side is
empty and the other is not; the run says so.

An interval counts from when go-fs starts, and the first run is then. A time
of day is in the time zone of the host; a time that a change to summer time
skips runs an hour later. A run never overlaps the one before it; one that
takes longer than the interval is followed by the next at once.

```toml
[[syncs]]
name = "photos"
enabled = true
protocol = "rsync"
server = "backup"
remoteFolder = "photos"
localFolder = "/photos"
direction = "both"
delete = true
at = "02:30"
days = ["mon", "wed", "fri"]
```
