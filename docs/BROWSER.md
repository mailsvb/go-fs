# Using go-fs from a browser

What the HTTP server offers a person in a browser, besides the listing itself.

## Logging in from a browser

Any HTTP account may log in through the page; programs keep using Basic or
Digest and are handed no session they did not ask for. A browser that has
to authenticate is sent to a login form of this server's own instead of being
challenged, so its native password box never appears: a `401` has to carry
`WWW-Authenticate` (RFC 9110 §15.5.2), and that header is precisely what raises
that box, so the answer is a redirect to a page that comes back `200`. The page
carries a **Log in** button when nobody is signed in, and the account name and a
**Log out** button when somebody is — and, for an account that sets `isAdmin`,
an **Admin** button that opens the
[web interface](CONFIGURATION.md#the-web-interface).

A browser is recognised by `Sec-Fetch-Mode`, which every current browser sends on
every request and no program sends, falling back to `text/html` in `Accept` for a
browser too old for it. A client that sends `X-Disable-Session` is treated as a
program. Where the configuration has no HTTP account at all there is nobody to
log in as, so there is no login page and a browser is challenged as a program is.

The login and logout endpoints have no paths of their own: they are
`?go-fs=login` and `?go-fs=logout` on the path being asked for. Every URL this
server answers is a path in the served folder — which is also why the page's
style and script are inlined rather than fetched — so a `/login` would shadow a
real name, while a query key can shadow nothing. It is also why there is no
`next` parameter to get wrong: the login page for `/private/` *is* `/private/`.

A successful login is carried by a **JSON Web Token** (RFC 7519) signed with
HMAC-SHA256 (RFC 7515) in the `goFsSessionToken` cookie, which is `HttpOnly`,
`SameSite=Lax` and `Secure` over TLS, scoped to `/`. The token carries
`iss`, `sub` (the account name), `aud`, `iat`, `nbf`, `exp`, `jti`,
`auth_time` (when the login happened) and a fingerprint of the credentials —
and **nothing else**. It deliberately does not
carry the paths or the rights: those are looked up from the configuration as it
stands on every request, so a token can never reach further than the account
behind it does right now, and a `paths` narrowed by a reload takes effect at
once. Verification pins the algorithm to HS256, so a token that says `alg: none`
or names an asymmetric algorithm is refused rather than trusted.

A login has two limits, both in seconds:

- `httpSessionTokenLifetime` is how long a browser stays logged in **without
  being used**, eight hours by default. A token is good for that long from the
  request that got it, and a request made in the second half of that window is
  answered with a fresh one. So a browser in use stays logged in, and one left
  alone is logged out. A page that is merely open does not count as use. It
  only sends requests while a fetch or registry job runs or its dialog is open.
- `httpSessionMaxLifetime` is how long a login lasts **at most**, counted from
  the login, a week by default. No renewal reaches past it, and a token from a
  login older than it is refused even if it has not expired.

These limits matter because a signed token cannot be withdrawn once it is out.
Logging out clears the browser's own copy, but a stolen token keeps working
until it expires, which is at most `httpSessionTokenLifetime` after it was
handed out. Things that cut a login short:

- those two limits;
- changing the account's password, which changes the fingerprint, so every
  browser logged in under the old one is signed out;
- removing the account;
- changing `httpSessionTokenSecret`, which signs out every browser at once.

**Behind a reverse proxy**, list it in `trustedProxies`, as an address or a
CIDR range. A request from one of them is recorded, counted and locked under
the client named in `X-Forwarded-For` — the rightmost entry that is not itself
a listed proxy, since the leftmost is whatever the client wrote — and
`X-Forwarded-Proto: https` marks the session cookie `Secure`, as the server's
own TLS listener does. From any other address both headers are the client's
word and are ignored, so without the setting every client behind a proxy
shares the proxy's address and the cookie is only `Secure` on `[https]`.

A browser that remembers Basic credentials sends them with every request.
Beside a session for the same account they count as that session, so the page
offers its logout and the admin interface opens; beside a session for another
account, or none, the header authenticates the request on its own.

`httpSessionTokenSecret` is the signing key, base64 of at least 32 random bytes:

```
head -c 32 /dev/urandom | base64
```

Leave it empty and one is generated at the first start and written into the
configuration file, as `shareLinkSecret` is, with the rest of the file left as
it was. That is what keeps a browser logged in across a restart, an update and
a change that rebuilds the HTTP server. If the file cannot be written, a key is
made for each run instead, which logs every browser out on a restart. Two hosts
serving the same folder share a login when they are given the same key. The
web interface will generate a new one for you. Changing it logs out every
browser.

`http.sessionTimeout` was what this used to be called, before the session became
a token that expires rather than a row in memory. A file that still sets it
loads, and says on startup that it is ignored.

## The trash

What is deleted over HTTP or HTTPS is not removed but moved to
`general.baseFolderTrash`: **Delete** in the listing, a `DELETE`, a WebDAV
client deleting a file or a folder or replacing one with `MOVE` or `COPY`, and
S3's `DeleteObject` and `DeleteObjects`, and a file a
[sync](REMOTE-SERVERS.md#synchronising-folders) removes from the served folder. It keeps the path it had, so
`/photos/2024/a.jpg` is `<trash>/photos/2024/a.jpg`, with its modification
time. Deleting the same path again replaces what the trash held for it; a
folder is merged into the one in the trash, its files replacing those of the
same name. FTP, SFTP and TFTP still delete for good, and so does everything
when `general.baseFolderTrash` is empty.

An account that sets `isAdmin` and has logged in through the browser finds
**Trash** in the menu of the header, just above **Admin**. It shows the trash
the way the listing shows a folder, with when each entry was deleted.
**Restore** in the row menu of a file or a folder moves it back to where it was
deleted from, making the folders above it that are gone. A file that is in its
place again is kept unless **Overwrite existing files with the version from the
trash** is ticked; what is kept stays in the trash. **Delete for good** removes
one entry, and **Empty trash** in the header everything; each of them asks
first. Nobody else is answered: a browser with no session is sent to the login
page, any other session is `403`, and the trash does not depend on
`enableAdminInterface`.

Once an hour, what has been in the trash longer than `general.trashRetentionDays`
(30 by default) is deleted for good and logged; `0` keeps it until it is
deleted on the page. When each path was deleted is kept in
`<trash>/.go-fs-trash.json`.

## Viewing a file

Clicking the name of a file the document viewer can show opens it over the
listing, in a window that fills the browser, instead of downloading it. The
viewer draws the file in the browser itself, with libraries go-fs carries in its
binary, so nothing is fetched from anywhere but go-fs:

| Files | Shown with |
|---|---|
| `.pdf` | [pdf.js](https://mozilla.github.io/pdf.js/) |
| `.docx`, `.docm`, `.xlsx`, `.xlsm`, `.pptx`, `.pptm`, `.ppsx` | [Office Open XML Viewer](https://github.com/yukiyokotani/office-open-xml-viewer) |
| `.csv`, `.tsv` | the same, as a sheet whose every field is the text it is |
| text and source code: `.txt`, `.log`, `.md`, `.ini`, `.conf`, `.yaml`, `.json`, `.xml`, `.html`, `.css`, `.sh`, `.go`, `.js`, `.ts`, `.py`, `.java`, `.c`, `.cs`, `.rs`, `.sql` and many more, and `Makefile`, `Dockerfile`, `README`, `LICENSE` and the like | [Ace](https://ace.c9.io), read only, coloured by the type of the file |
| `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.avif`, `.bmp` | the browser |
| `.mp4`, `.m4v`, `.webm`, `.ogv`, `.mov`, `.mkv` | [Video.js](https://videojs.com), played by the browser (see [Playing audio and video](#playing-audio-and-video)) |

Whatever draws it, the window looks the same: the name, the page or slide it is
at, or the sheet, the zoom with *Fit to width* and *Fit the page*, **Download**
and **Close**. ←/→ go a page back and forth where the file does not scroll
sideways, `+`, `-` and `0` zoom, and Escape closes it. A PDF or an Office file
that needs a password asks for it. Opened from its name, the viewer only reads:
nothing is changed (see [Editing a text file](#editing-a-text-file) for the
one way it writes), a script in a PDF never runs, and a link in a document
opens the web or a mail in a tab of its own, never anything else.

An older Office file, `.doc`, `.xls` or `.ppt`, is not shown, nor is an SVG,
which is a page that can run script. Of a text file the first 10 MB are shown.
Markup such as `.html` is shown as its source, never as the page it makes.
A click with Ctrl, Cmd or Shift, or with the middle button, does what the link
does without the viewer.

Without JavaScript, or in a binary without the viewer, the name of a PDF or a
text file opens it in a new tab instead, and any other file is downloaded when
its name is clicked, as before. **Download** in a row's menu always downloads,
whatever the type.

The viewer reads the file from `?go-fs=view` on it (`&view=1` in the listing of
a stored server), which needs the right a download needs, answers ranges the
way a download does, and is logged as `http view`. Without the marker a `GET` is
the download it always was, so programs see no difference. A PDF or a text file
asked for with it is sent to be shown in the tab; any other type is sent as a
download, which the viewer reads all the same.

### Playing audio and video

A video opens in the same window as any other file, filling it, and starts
playing; audio plays in a bar at the bottom of the window instead, which stays
there while you browse other folders. Both are played by the browser itself,
with [Video.js](https://videojs.com) for the controls: nothing is converted,
so what plays is what the browser can play, and the listing asks it before it
offers a file. Whatever it cannot play is downloaded when its name is clicked,
as before.

| Kind | Files | Usually played by |
|---|---|---|
| Video | `.mp4`, `.m4v`, `.webm`, `.ogv`, `.mov`, `.mkv` | every current browser for H.264 or VP9 in `.mp4` and `.webm`; `.mov` and `.mkv` where the browser takes them, which for H.264 is Chrome, Edge and Safari for `.mov`, and Chrome and Edge for `.mkv` |
| Audio | `.mp3`, `.m4a`, `.aac`, `.ogg`, `.oga`, `.opus`, `.flac`, `.wav`, `.weba` | every current browser; Safari takes `.ogg` and `.opus` only in recent versions |

An `.avi`, `.wmv` or `.flv` is never offered, since no browser plays them. A
file whose container the browser takes but whose codec it does not, an `.mp4`
with H.265 in a browser without it for one, says that the browser cannot play
it, with **Download** offered instead.

**Video** has play and pause, the time, a progress bar to seek on, the speed,
the volume, picture in picture and full screen. Space or K plays and pauses,
←/→ seek 5 seconds, ↑/↓ change the volume, M mutes, F goes full screen, and
Escape closes the window.

**Audio** starts with the file you clicked; the other audio files of that
folder, in the order the listing shows them and without those the filter
hides, become the queue. The bar shows the name and where it is in the queue,
with previous, play and pause, next, the time, a progress bar, the speed and
the volume, and moves on to the next file when one ends. Previous goes back to
the start of the file once it has played a few seconds, and to the one before
otherwise. A file that does not play is passed over. Clicking another audio
file starts a new queue from its folder. The × closes the bar.

While the bar plays you can browse: open folders, go back and forward with the
browser, upload, rename and delete, and the music goes on. The first time the
page would otherwise be left, it keeps itself and shows the next page in a
frame above the bar, and the address bar and the title follow that frame, so
a link copied from the address bar is the folder you are in. Reloading the
page, closing the tab or logging out stops the music. A video opened while
audio plays pauses it.

A video is not read into the page whole, as a document is: the browser asks
for the parts it plays and seeks to as ranges of `?go-fs=view`, each of which
is logged as an `http view`.

### Editing a text file

**Edit…** in the menu of a row opens a text file, or a `.csv` or `.tsv`, in the
same window with the editor writable. It is offered on the files the viewer
shows as text, to an account that may both read the file and replace it
(`allowUserFileOverwrite`), and only on the served folder, not on the page of a
stored server.

**Save**, or Ctrl+S (Cmd+S on a Mac), writes the file back with a `PUT`, as an
upload that replaces it does: the same right, the same `maxUploadSize`, the
same log record and the same respect for a lock a WebDAV client holds. A dot
beside the name says there are changes not saved; closing the window, or
Escape, with such changes asks whether to save them, discard them or keep
editing. Once a file was saved, closing the window reloads the listing so that
its size and time are current.

The file is saved as it was read: UTF-8, or UTF-16 when it began with the mark
that says so, with a byte order mark where it had one, and with the line
endings it had. A file that is not valid text in its encoding, or that is
larger than the 10 MB the viewer reads, is shown but not editable, and the
window says why.

The save is made on the condition that nobody changed the file since it was
opened: the `PUT` carries `If-Unmodified-Since` with the `Last-Modified` the
file was read with, and the server refuses it with `412 Precondition Failed`
when the file is newer. The editor then asks before saving over the other
change. Every `PUT` that replaces a file honours the header, and answers with
the `Last-Modified` of what it wrote; a client that does not send it is not
affected. The time is to the second, so two saves within the same second are
not told apart.

### What runs where

A file is shown from the listing's own origin, which carries the session. So
what is shown must never run script there: an uploaded `.html` or `.svg` that
did would act as whoever opened it. Text opened in a tab is therefore always
sent as `text/plain; charset=utf-8`, even `.json` and `.xml`, and markup is not
opened in a tab at all; the viewer shows it only as text in the editor. Every view also carries `X-Content-Type-Options: nosniff` and
`Content-Security-Policy: sandbox; default-src 'none'`, which gives the document
an origin of its own. Browsers show a PDF in that sandbox as they do without it.

The viewer itself is a page of go-fs in a frame of the listing, with a policy of
its own: script only from go-fs, with WebAssembly for the parsers of the
libraries, and no fetch, image, font or media from any other host. The editor runs no
workers and lays out text as text. The bar that plays audio is a page of the
viewer as well, with the same policy, and plays only files of go-fs; while it
plays, the listing shows the pages one browses to in a frame of its own origin. A document that
got a library to write markup still could not run script, and it reaches
nothing outside go-fs.

### The folder `/.go-fs`

The viewer and its libraries are served below `/.go-fs/viewer/`, under a name
that changes with every build, so that a browser keeps them for as long as it
likes. They load their parts by paths relative to their own, which a query on
a path of the served folder, as every other endpoint is, cannot give them.

So a folder `.go-fs` at the top of `general.baseFolderFiles` is go-fs's own, on
every protocol: HTTP, WebDAV and S3, FTP, SFTP and TFTP neither show it nor let
anyone create, read, change or remove anything in it, in any spelling the
filesystem takes for it, nor through a link to it. A sync of the whole served
folder leaves it out on both sides. A folder of that name further down is an
ordinary folder. A served folder that has one at its top already, from before
go-fs reserved it, keeps it as it is; go-fs says so in its log when it starts.

The libraries go-fs embeds are [pdf.js](https://github.com/mozilla/pdf.js)
under the Apache License 2.0 and the
[Office Open XML Viewer](https://github.com/yukiyokotani/office-open-xml-viewer)
under the MIT License, with the fonts and parsers they bring,
[Ace](https://github.com/ajaxorg/ace) under the BSD 3-Clause License, and
[Video.js](https://github.com/videojs/video.js) under the Apache License 2.0.
Their licenses are served beside them, at `/.go-fs/viewer/<build>/pdfjs/LICENSE`,
`/.go-fs/viewer/<build>/ooxml/LICENSE` with `THIRD_PARTY_NOTICES.md`,
`/.go-fs/viewer/<build>/ace/LICENSE` and `/.go-fs/viewer/<build>/videojs/LICENSE`, and
`/.go-fs/viewer/<build>/VERSIONS` says which versions the build has.

## Downloading a folder

**Download** on a folder in the listing saves the whole folder as
`<folder>.tar.xz`. The endpoint is `?go-fs=archive` on the folder (`GET`, or
`HEAD` for the headers alone), so a program can ask for it too:

```shell
curl -u john:doe -o photos.tar.xz 'https://example.com/photos/?go-fs=archive'
```

The archive is packed while it is sent, so nothing is staged on the server and
the answer carries no `Content-Length`. It needs the right that listing the
folder needs, and what goes into it is decided entry by entry as a `GET` of that
entry would be: a sub-folder that `pathsRequireAuth` or the account's `paths`
keep from the requester is left out. A symbolic link goes in as the file it
points to while that file is inside `general.baseFolderFiles`; a link to a folder, or to
anything outside, is left out. Compressing takes one CPU core of the server:
text and other data that compresses well packs at around 80 MB/s, but data that
does not — video, images, archives — at well under 10 MB/s, so a large folder of
those takes a while. `writeTimeout`, where set, caps an archive the way it caps
any download.

## Sharing a file

**Share…** in the row menu of a file hands out a link that downloads that
one file without an account, for passing a protected file to someone who has
none. The dialog shows the full link to copy. It is the file's own URL with
`?key=` added:

```text
https://example.com/osem/file.extension?key=3f9c…  (128 hex characters)
```

The download names the file in its `Content-Disposition` header, which a
browser follows. Command-line clients only use the header when asked to.
Without that, `wget` names the file after the URL, key included:

```shell
wget --content-disposition 'https://example.com/osem/file.extension?key=3f9c…'
curl -OJ 'https://example.com/osem/file.extension?key=3f9c…'
```

The key is an HMAC-SHA512, keyed by `http.shareLinkSecret`, over the file's
path, its modification time and its size. Nothing is stored on the server: the
key is computed again from the file at every request, so a link stops working
as soon as the file is changed, touched, renamed or replaced. A link only reads.
It answers `GET` and `HEAD` of the file it was made for, and nothing else: a
folder, another file or any other method with the key is authenticated as it
would be without it. A key that no longer matches is ignored, so a signed-in
account holding an old link still gets the file.

Any account that is signed in, with a session, Basic, Digest or a bearer token,
and may read the file can make a link. The endpoint is `?go-fs=share` on the
file and answers the path and query of the link as JSON:

```shell
curl -u john:doe 'https://example.com/osem/file.extension?go-fs=share'
{"path":"/osem/file.extension?key=3f9c…"}
```

`http.shareLinkSecret` is generated and written into the configuration file at
the first start, with the rest of the file left as it was. The admin interface
shows it read-only. Changing it by hand revokes every link handed out so far.
If the file cannot be written, a key is made for this run only, and the links
stop working at the next restart.

## Fetching a file from a URL

An account that has logged in through the form, and may create files in a
folder, has a **Fetch** button at the top right of that folder's listing. The
dialog takes an `http://` or `https://` URL, an optional username and password
(or token), sent as Basic, and, folded away under **HTTP headers**, any headers
to send along, one `Name: value` per line, such as `Accept:
application/octet-stream` or `PRIVATE-TOKEN: …`. Headers the connection
decides itself (`Host`, `Content-Length`, `Connection`, `Proxy-*` and the
like) are refused, and so is an `Authorization` header next to a username.
**Validate Connection**, ticked by default, checks the TLS certificate of an
`https://` URL; untick it to accept whatever certificate the remote shows,
such as a self-signed one. It holds for the redirects as well.

The server downloads the file itself, in the background, as the registry page
pulls an image, and finishes it whether or not a page is still open: closing
or reloading the page does not stop it. The dialog waits only until the remote
has answered; an error such as a `404` or a name that is taken is shown there,
and once the download has begun the dialog closes. Several downloads can run
side by side. While any runs, a button left of the filter, in every listing of
the account, shows how fast they go together, and its background fills from
left to right as far as they have got. It opens **Transfers**, which lists
each with its name, the share that has arrived, its speed and **Stop**, which
stops it and takes it off the list; a download that failed stays there with
its reason until it is cleared. When one finishes into the folder on the
screen, the page reloads to show the file; elsewhere the banner says so.
Redirects are followed, ten at most, and the login and an `Authorization`
header go only to the scheme, host and port they were given for, while the
other headers follow the redirect. The file is stored as sent: a body with a
`Content-Encoding` is not decoded.

The file is named by the `filename` of the answer's `Content-Disposition`
where there is one, and by the last segment of the URL the redirects ended at
otherwise; only the last segment of either is used, so the file always lands
in the folder shown. A name that is taken is replaced only for an account that
sets `allowUserFileOverwrite`, as for an upload. `maxUploadSize` applies, and
the bytes are staged in `general.baseFolderTemp` until the download is
complete. At most eight fetches run at once, all accounts together, apart
from the registry's pulls and pushes, of which at most four do. Sends over
SFTP (see [Remote servers](REMOTE-SERVERS.md#sending-a-file-to-another-server)) count towards the same eight.

Fetch is offered to a session only: not to Basic, Digest or bearer
credentials, which have `PUT` for the same job, and not to anyone where `PUT`
is public. The endpoints are `?go-fs=fetch` (`POST` of `url`, `username`,
`password`, `headers` and `skipVerify`) and
`?go-fs=fetch-job&id=…` (`GET` for the progress, `DELETE` to stop and clear)
on the folder, and `?go-fs=fetch-jobs` (`GET`), on any folder, for the
account's fetches and sends that have not been cleared, with `kind`, `name`, `folder`,
`phase` (`downloading` once the remote has answered), `bytesDone`,
`bytesTotal` and `bytesPerSecond`. Note that the server connects to whatever host is typed in, from its
own network, internal addresses included: give the right to create files only
to accounts that may do that.
