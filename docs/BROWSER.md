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
the volume, and moves on to the next file when one ends. The speed steps
through 0.75×, 1×, 1.25×, 1.5× and 2× with each click. Previous goes back to
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

## Editing MP3 tags

**Edit MP3 Tag…** in the menu of an `.mp3` file opens an editor over the page
for the tag and the volume of that file. **Edit MP3 Tags** in the header opens
the same editor for every `.mp3` file of the folder as a table, one row per
file. Both are offered to an account that may replace a file there
(`allowUserFileOverwrite`). On the page of a stored server they need the
server's `allowUpload` too, as replacing a file there does.

The fields are Title, Artist, Album Artist, Album, Year, Track and Genre, with
the common genre names suggested, and the front cover, which a JPEG or PNG
image of up to 10 MiB replaces. In the folder editor, a value typed into the
first row is set on every file, and emptying it again gives every file its own
value back. **1, 2, 3…** numbers the files in the order of the table, as
`1/12`, `2/12` and so on.

A save changes only these fields and the volume information. Every other
frame of the tag — comments, the disc number, the composer, lyrics, other
pictures, other `TXXX` frames — stays as it is, and so do APEv2, Lyrics3 and
ID3v1 tags at the end of the file; an ID3v1 tag gets the new values of its
fields too. The tag is written as ID3v2.3. A file of the served folder is
written beside itself and renamed into place, so it is never half written. A
file of a stored server is uploaded again whole, as an upload that replaces it
is. Each save carries `If-Unmodified-Since`, so a file somebody changed since
the editor read it is not overwritten (`412`), and it respects the lock a
WebDAV client holds.

### The volume

The volume changes without decoding and encoding the audio again, as
[MP3Gain](https://mp3gain.sourceforge.net/) does. Every MPEG Layer III frame
stores a global gain per channel, and the editor adds the same number of steps
to all of them. One step is 1.5 dB. The coded audio itself is left as it is, so
the same number of steps the other way restores the file exactly. Frames of
digital silence, with a global gain of 0, are left as they are, as MP3Gain
leaves them.

The change is recorded as MP3Gain records it: in the item `MP3GAIN_UNDO` of an
APEv2 tag at the end of the file, which holds the change that restores the
original level. The editor shows how many steps were applied so far, also by
MP3Gain, and `mp3gain -u` undoes a change made in go-fs. A change made with
`mp3gain -s i`, which keeps the item in the ID3v2 tag instead, is read too, and
so is the `TXXX` frame `GO-TAG_GAIN_STEPS` of earlier versions, which the next
change replaces. A change removes what MP3Gain keeps about a file's gain values
and loudness (`MP3GAIN_MINMAX`, `MP3GAIN_ALBUM_MINMAX` and the `REPLAYGAIN_*`
items of the APEv2 tag), which no longer apply and which MP3Gain measures again.
`mp3gain -u` does not update the ReplayGain values of the ID3v2 tag, so analyze
a file again after undoing a change with MP3Gain.

**Analyze** decodes the file, measures its loudness (EBU R128 /
ITU-R BS.1770) and its peak, and suggests the change that brings it to
−18 LUFS, the reference of ReplayGain 2.0. The suggestion is smaller where the
full change would make the audio clip, or where the gain of the frames
allows no more. The change is typed in steps, or taken from the suggestion.
In the folder editor, **Analyze all** measures every file. **Each file to
−18 LUFS** changes each by its own amount; **As one album** changes all of them
by the same amount, so that the album as a whole reaches −18 LUFS and its tracks
keep their levels to each other. A further number of steps can be added to
every file on top of that.

A save after a measurement also writes the ReplayGain values that are left
after the change (`REPLAYGAIN_TRACK_GAIN` and `_PEAK`, and for an album
`REPLAYGAIN_ALBUM_GAIN` and `_PEAK`). A change of the volume without an album
removes the album values, which no longer apply.

Measuring decodes the whole file, which takes a core for about as long as the
decoder needs; go-fs measures at most as many files at once as the machine has
cores, and refuses files larger than 512 MiB. A Jellyfin library reads a file
again once it was saved, as it does any changed file.

What a measurement found is kept in the
[persistent data file](INSTALL.md#run), not in the file: measuring never
writes to a file, and only a save writes the ReplayGain values. Analyze, and
Analyze all, then answer at once for a file measured before, also after a
restart, for as long as the file stays as it is; the editors say which files
that is. A save keeps the measurement of the file it changes, moved by the
change of the volume, unless the louder audio would clip. A measurement nobody
used for 180 days is dropped.

The editor is left out of go-fs-light, and of a binary built with the tag
`no_mp3tags`.

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
entry would be: a sub-folder that `pathsWithoutAuth` leaves out, or the
account's `paths` keep from the requester, is left out. A symbolic link goes in as the file it
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

Below the link the dialog shows the `curl` and `wget` commands that fetch it,
each with a Copy button. The address in front of the path is the one the page
was opened at, unless `general.internalAddress` or `general.externalAddress`
names another (see [Configuration](CONFIGURATION.md)). With one of them set,
every link uses that one. With both, an **Address** dropdown above the lines
switches all three between the internal and the external address. It starts on
the one the page was opened at, matched by origin and then by host name, and on
the external one when the page was opened at neither, say through `localhost`.

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

## Playing on Sonos

With `http.sonos = true`, **Play on Sonos…** in the row menu of an `.mp3`,
`.m4a`, `.aac`, `.flac`, `.ogg`, `.oga` or `.wav` file plays it on a Sonos
speaker of the local network. The dialog lists the rooms the server finds, a
group of rooms that play together as one entry named after its first room.
**Play this file** has the room chosen play the file, in place of what it
played; the queue of the Sonos app stays as it was. Below the list the dialog
shows what the room is doing, with **Resume**, **Pause**, **Stop** and the
volume of the whole group. **Search again** looks for speakers again, say after
rooms were grouped in the Sonos app. The room chosen last is chosen again the
next time the dialog opens, in the same browser.

### A folder on Sonos

The audio bar at the bottom of the window has a speaker button where Play on
Sonos is offered. It opens the same dialog on the bar's queue: the file the
bar plays and the audio files after it, in the order the listing shows them.
**Play N files** hands the queue to the server, which plays it on the room or
group chosen, one file after the other, by itself. The browser stops playing,
and the bar becomes the remote control of the speakers: what they play, how
far into it, the file's place in the queue, **Previous**, **Pause** or
**Play**, **Next**, the volume, and **Stop**. **Play on Sonos…** in the row
menu does the same with the one file.

The browser may then be closed. The listing opens the bar on what the speakers
play again the next time it is opened, in any browser the account is logged
in with. Playing a file in the browser takes the bar back; the speakers play
on, and the bar shows them again on the next page opened. Closing the bar does
not stop the speakers either; its **Stop** does.

Since a speaker says nothing when a file ends, the server asks the group what
it does every two seconds, twice a second near the end of a file, and tells
it the next file once the last one has ended. That leaves a moment of silence
between files. The queue ends:

- after its last file; a file that is gone by then is passed over,
- with **Stop**, in the bar or in the dialog, or with Stop in the Sonos app
  before a file's end,
- when the group is told to play something else, in the Sonos app or with
  another Play on Sonos on one of its speakers,
- when its speakers do not answer five times in a row, or stay paused for
  six hours,
- when `http.sonos` is switched off or the account may no longer read the
  files,
- and with go-fs: a restart lets the file that plays play to its end, and
  nothing after it.

Tick several entries and **Play together** has them all play the file in step.
They do so the way the Sonos app groups rooms: the other entries join the one
marked *leads* (the one played on last, or else the first ticked), and play
whatever it plays. The rooms stay grouped afterwards, in the Sonos app too,
until they are grouped otherwise. **Ungroup**, under the list, has every room
of the group that leads play on its own again. A room that does not join, say
because it is switched off, is named in the dialog, and the others play
anyway.

A speaker is not sent the file. It is told a link, which it then fetches from
this server itself, with no account:

```text
http://192.168.1.5:8080/music/song.mp3?go-fs-play=1767225600.3f9c…
```

The link goes to `general.internalAddress` where that is set, or else to the
plain HTTP listener, or the HTTPS one when there is no plain one, at
`http.address`. Where that is `0.0.0.0`, the address is the one this host
reaches the speaker from. The speakers have to reach it: with `http.address`
at `127.0.0.1` the dialog refuses to play. A speaker fetches over HTTPS only
from a certificate it trusts, which go-fs's own certificate is not, so serve
the speakers over plain HTTP on the local network or name a trusted address in
`general.internalAddress`.

The key is an HMAC-SHA256, keyed by `http.shareLinkSecret`, over the file's
path, the time it runs out, and the file's modification time and size. Like a
share link it answers only `GET` and `HEAD` of the one file, with ranges, so a
speaker can seek. Unlike a share link it runs out by itself, twelve hours after
it was handed out. It stops working as soon as the file changes, and while
`http.sonos` is off.

The server finds the speakers by SSDP: it sends a search to
`239.255.255.250:1900` on every interface that takes multicast and waits two
seconds for the answers. It then asks one speaker of each household how the
rooms are grouped. When neither the search nor `http.sonosSpeakers` finds a
speaker, the server tries port 1400 on every address of the private IPv4
networks it is on, a /22 at most and else the /24 it is in, and keeps the
addresses that describe themselves as Sonos speakers. That finds them where
the router does not pass multicast from the wired network to the wireless
one, or where the server's firewall drops the answers to the search, which
come from the speakers rather than from the group the search went to. A
network the server is not on, a VLAN of its own say, is reached by naming its
speakers in `http.sonosSpeakers`, as `"192.168.20.31"` or
`"192.168.20.31:1400"`, which are asked directly; one speaker of a household
is enough, as it knows every room. What
was found is kept for 30 seconds. The server only talks to the speakers it
found or that `http.sonosSpeakers` names, and believes an answer to its search
only where the speaker's description is on the address the answer came from,
at port 1400. The dialog names a room by its id and never by an address.

Playing a file is offered as **Send File…** is: to a session of an account that
may read the file. The endpoint is `?go-fs=sonos&sonos=<what>` on the file:
`groups` and `state` (with `&group=<id>`) answer `GET`, and `play`, `resume`,
`pause`, `stop`, `volume` and `ungroup` take a `POST` of `{"group":"<id>"}`,
with `"volume"` from 0 to 100 for `volume`. `play` also takes `"join"`, the ids
of more groups whose speakers join the group first, and `"files"`, the names of
files of the folder to play in order from this one, which holds its name; its
answer names the group as it is then, lists in `"failed"` the rooms that did
not join, and has the queue's `"session"`. The bar asks about that on the
folder: `session&id=<id>` answers `GET` with what the speakers do, and `skip`,
with `{"session":"<id>","index":<n>}`, and `end`, with `{"session":"<id>"}`,
take a `POST`. A queue is the account's own; another account is told it is not
there.

Playing on Sonos is left out of go-fs-light, and of a binary built with the
tag `no_sonos`.

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
`password`, `headers`, `skipVerify` and `media`, see below) and
`?go-fs=fetch-job&id=…` (`GET` for the progress, `DELETE` to stop and clear)
on the folder, and `?go-fs=fetch-jobs` (`GET`), on any folder, for the
account's fetches and sends that have not been cleared, with `kind`, `name`, `folder`,
`phase` (`downloading` once the remote has answered, `converting` while
a video or audio that was downloaded is joined or converted), `bytesDone`,
`bytesTotal` and `bytesPerSecond`, and while an audio is converted to MP3
`convertDone` and `convertTotal`, how far ffmpeg is in the audio and how long
it is in milliseconds, and `convertSpeed`, how many times faster than it plays. Note that the server connects to whatever host is typed in, from its
own network, internal addresses included: give the right to create files only
to accounts that may do that.

### Video and audio

While go-fs has found [yt-dlp](https://github.com/yt-dlp/yt-dlp) (see
[External tools](CONFIGURATION.md#external-tools)), the Fetch dialog has
**Download as** below the URL: **File, as the URL serves it** is the fetch
above, **Video** and **Audio only** have yt-dlp take the video, or its sound,
out of the page the URL names, such as a YouTube video. Typing a YouTube link
(`youtube.com`, `m.`/`music.youtube.com`, `youtu.be`) picks **Video** by
itself; yt-dlp knows many other sites as well, which are fetched by choosing
**Video** or **Audio only** by hand. YouTube links are fetched over IPv4:
over many IPv6 connections, home ones included, YouTube answers yt-dlp with
`Sign in to confirm you're not a bot`, while over IPv4 it needs no login or
cookies.

What is fetched depends on whether go-fs also found ffmpeg:

| | with ffmpeg | without ffmpeg |
|---|---|---|
| Video | the best video and the best audio, joined into an `.mp4` | the best single file that holds both, often of a lower resolution |
| Audio only | the best audio, converted to an `.mp3` of the highest VBR quality, tagged and with a cover | the best audio as the site sends it, an `.m4a` where it offers one |

The `.mp3` is tagged (ID3v2.3) with what yt-dlp knows of the video: the
title, or the track where the site names one; the artist, which most videos
do not name, so their channel stands in for it; the album, its artist and
the track number where the site has them, as YouTube Music does; the year it
was released, or else uploaded; and the address of the video as the comment.
Its cover is the thumbnail of the video in its own shape, shrunk where its
longer side is more than 768 pixels. An audio the site sends as an MP3
already is tagged without being converted again, and a thumbnail ffmpeg
cannot read leaves the file without a cover rather than fail the fetch. The tags can be changed afterwards with
[Edit MP3 Tag…](#editing-mp3-tags).

The file is named `Title [id].ext` after the video, and lands in the folder
under the rules of any fetch: created where the account may create, replacing
a file only with `allowUserFileOverwrite`, and no larger than `maxUploadSize`.
It runs as a fetch too, among the eight that run at once, in **Transfers**
with its progress, its speed and **Stop**; the progress of a video counts its
video and its audio together, which are downloaded one after the other.
Once the download is complete, ffmpeg takes a while longer. An audio to be
an MP3 is converted by go-fs itself, which runs ffmpeg with `-progress`
rather than leave it to yt-dlp, which tells nothing until ffmpeg ends: the
bar starts again from 0% in amber and reads, say, **Converting · 24×**, how
many times faster than it plays the audio is converted; for a long mix on a
slow machine that is minutes. Joining a video and its audio copies them and
takes seconds, shown as **Converting…** at 100%. The
dialog waits until yt-dlp has found the video and begun the download, so an
error such as `Video unavailable` or a video that needs a login is shown
there.

yt-dlp downloads into a folder of its own in `general.baseFolderTemp`, which
is removed when the fetch ends, however it ends; Stop ends yt-dlp together
with the ffmpeg it runs. It keeps a cache of what it learned of a site in
`yt-dlp-cache` there. It is run without a shell, with `--ignore-config`, so no
configuration file of yt-dlp changes what it does, and with `--no-playlist`
and one download at most: the link of a video in a playlist fetches that
video. A video or audio fetch takes no username, password or headers, which
yt-dlp would have on its command line, visible to every user of the machine.

The endpoint is the same `?go-fs=fetch`, with `media` set to `video` or
`audio`; it answers `400` while there is no yt-dlp.

Whether a video may be downloaded is up to its site's terms and to copyright,
which go-fs does not check: offer this to accounts that respect them.
