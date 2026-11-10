# Repositories

go-fs can host an rpm repository for zypper and dnf, an
[F-Droid repository](#f-droid-repository) for Android, or a
[music library for Jellyfin clients](#jellyfin-music-library), in a folder it
serves over HTTP, and keep it up to date by itself. Put an rpm into the folder over any of
the servers (an HTTP `PUT`, S3, FTP, SFTP, TFTP) or copy it there on disk. At
the next look at the folder, go-fs:

1. **checks** the package against the digests it carries. A package still being
   uploaded fails that check, is left out and is tried again at the next look.
2. **removes old versions**: per package name and architecture, only the newest
   `keep` versions by epoch, version and release stay, and older files are
   deleted. Each removal is logged.
3. **signs** each package that is not signed with the repository's key yet. It
   replaces any other signature and writes the file anew in place, leaving the
   header and payload untouched.
4. **writes the metadata**, as `createrepo_c` does: `repodata/` with
   `primary.xml.gz`, `filelists.xml.gz`, `other.xml.gz` and `repomd.xml`,
   signed as `repomd.xml.asc`, with the public key as `repomd.xml.key`.

The metadata is written only when the set of packages changed. The lists are
named by their checksum and `repomd.xml` is replaced last, so a client never
reads half a repository. The lists of the build before are kept for one more
build, for a client that read the old `repomd.xml` a moment earlier.
Subfolders are looked into, except `repodata` and those whose name starts
with a dot. Source rpms are left out.

```toml
[[repositories]]
name = "go-fs"                  # the id clients give it
type = "rpm"
folder = "/rpm/go-fs"           # in general.baseFolderFiles, as the listing shows it
signingKey = "…"                # armored OpenPGP private key, base64, no passphrase
keep = 5                        # versions per package and architecture; 0 keeps all
pollInterval = 60               # seconds between two looks at the folder
url = "https://files.example.com/rpm/go-fs"   # optional, see below
```

They are added and edited on the **REPOSITORY** tab of the admin interface. Its
dialog makes the folder when it is not there yet, and generates an RSA 4096
signing key or takes an uploaded one. An uploaded key has to be armored
(`gpg --armor --export-secret-keys <id>`) and have no passphrase, because go-fs
signs with nobody there to type one. Only RSA keys are accepted, because older
rpm versions still in use do not verify EdDSA signatures. The repository is
written to the file at once and starts at the next reload. Its record shows how
many packages it lists, when it last looked, the files it left out and why, and
the commands a client adds it with.

With `url` set, the repository also writes `<name>.repo` into its folder:

```shell
# openSUSE and SLES
sudo zypper addrepo https://files.example.com/rpm/go-fs/go-fs.repo
# Fedora, RHEL and Rocky Linux
sudo dnf config-manager addrepo --from-repofile=https://files.example.com/rpm/go-fs/go-fs.repo
```

zypper and dnf read the folder without logging in, so it has to be public:
listed in `pathsWithoutAuth`. Uploading to it is protected as any upload is.

## F-Droid repository

With `type = "fdroid"` the folder is a repository of APKs that the
[F-Droid](https://f-droid.org) app adds as a third-party repository. It works
like an rpm repository, with these differences:

- **The APKs are not signed again.** They keep their developer's signature,
  which Android checks when it installs one. An APK that is not signed at all
  is left out. What is signed is the index.
- **`keep`** counts versions per package name, by `versionCode`.
- **The index** is version 2 of the F-Droid format: `index-v2.json`, and
  `entry.json` signed into `entry.jar`. The app checks `entry.jar` against the
  fingerprint the repository was added with. The launcher icons of the apps are
  extracted into `icons/`. F-Droid apps from before 2023 read only the first
  version of the index, which is not written.
- **`signingKey`** is a self-signed certificate and its RSA private key, PEM,
  one after the other. The dialog generates one (RSA 4096, valid for 10000
  days, as `fdroid init` makes it), or takes an uploaded PEM file. The
  repository's fingerprint is the SHA-256 of that certificate, so keep the key
  once clients use the repository.
- **`url` is required and has to end in `/repo`**, for example
  `https://files.example.com/android/repo` for the folder `/android/repo`.
  Given any other address, the app looks for the repository at
  `<address>/fdroid/repo`.

```toml
[[repositories]]
name = "android"
type = "fdroid"
folder = "/android/repo"
signingKey = "…"                # certificate and RSA private key, PEM, base64
keep = 3                        # versions per package name; 0 keeps all
url = "https://files.example.com/android/repo"
title = "Example apps"          # optional, the name the app shows; name when not set
description = "The apps of example.com."   # optional
icon = "/icon.png"              # optional, an image in the folder
```

The repository's record on the REPOSITORY tab shows its address with the
fingerprint, `https://files.example.com/android/repo?fingerprint=<SHA-256>`.
Add that in the F-Droid app under Settings › Repositories, or open the
`fdroidrepos://` link the record gives on the phone.

The app's name and icon come from the APK. More can be put beside the APKs in
the fastlane layout, per app and language, and every file is optional:

```text
<folder>/<packageName>/<locale>/          e.g. org.example.app/en-US/
  title.txt                 name, instead of the APK's label
  short_description.txt     summary, up to 80 characters
  full_description.txt      description
  video.txt                 address of a video
  changelogs/<versionCode>.txt, changelogs/default.txt
  icon.png, featureGraphic.png, promoGraphic.png, tvBanner.png
  phoneScreenshots/, sevenInchScreenshots/, tenInchScreenshots/,
  tvScreenshots/, wearScreenshots/
```

fdroidserver's names (`name.txt`, `summary.txt`, `description.txt`) work too.
The default language is `en-US`. A change to these files is picked up at the
next look, as a new APK is. As for rpm, the folder has to be public for
reading.

## Jellyfin music library

With `type = "jellyfin"` the folder is a music library that
[Jellyfin](https://jellyfin.org) clients such as Finamp, Symfonium, Feishin and
the Jellyfin apps browse, play and download from, as they would from a
Jellyfin server. go-fs is not a Jellyfin server: it answers the part of the
API those clients need for music, and plays every file as it is. Nothing is
transcoded, so a client gets the FLAC or Opus file itself and has to be able
to play it.

```toml
[[repositories]]
name = "music"
type = "jellyfin"
folder = "/music"
title = "Our music"             # optional, the library's name in the clients; name when not set
url = "https://files.example.com"   # optional, the address the admin page tells clients to add
```

It differs from the package repositories in these ways:

- **Nothing is removed or signed.** There is no `signingKey`, and `keep` is 0.
  The audio files are `.mp3`, `.flac`, `.m4a`, `.aac`, `.ogg`, `.oga`,
  `.opus` and `.wav`, in any folder below the library's.
- **The index is kept in memory**, not written into the folder. It is built
  at start and again whenever a file or a cover changes. Albums, artists and
  genres come from the tags: ID3, Vorbis comments, MP4. A file without tags is
  named after itself, its album after its folder, and its artist after the
  folder above. Tracks with an album artist tag are grouped by album artist
  and album. Tracks without one are grouped by album within one folder, so a
  compilation stays one album, by Various Artists.
- **Covers** are `cover`, `folder`, `front` or `album` `.jpg`, `.jpeg`,
  `.png` or `.webp` beside the tracks. Otherwise the picture embedded in a
  track is used. Images are sent at their own size. A library and a genre
  show the cover of their first album that has one. **Covers are sent without
  a token**, as Jellyfin sends them, because Finamp asks for them without one:
  anyone who knows the id of an item gets its cover, whatever the `paths` of
  an account. A request with a token gets only the covers its account may
  read.
- **The API is answered at the root of the HTTP server.** In the client, add
  `https://files.example.com`, the server rather than the folder. Requests
  that come from a Jellyfin client are recognized by the `MediaBrowser`
  Authorization header or the token they carry, so a browser still reaches
  the files. Only the few paths a client asks before it has logged in are
  always the API: `/System/Info/Public`, `/System/Ping`, `/Users/Public`,
  `/Branding/…`, `/QuickConnect/Enabled`, and the covers,
  `/Items/{id}/Images/…`. go-fs logs a warning when the
  served folder has a `System`, `Users`, `Branding` or `QuickConnect` folder
  those paths would hide.
- **Clients show the server by the libraries' names** and the version of
  go-fs, such as `Our music · go-fs 1.2.0`. The version it reports as a
  Jellyfin server stays one of Jellyfin, because clients refuse a server they
  take to be too old.
- **Clients on the local network can find the server** with
  `http.jellyfinDiscovery = true`. Finamp and the Jellyfin apps ask for
  servers by a broadcast to UDP port 7359, and go-fs answers with the `url`
  of the library, or else its own address on the network the question came
  from. To receive a broadcast it listens on every interface, unless
  `http.address` is `127.0.0.1`, where no other host finds it. A Jellyfin
  server on the same host holds that port already; go-fs then logs a warning
  and serves on without it.
- **Clients log in with the accounts of go-fs** that set `http`. A client is
  given a token that lasts a year and stops working when the account's
  password changes, or when the client logs out with it. An account sees the tracks it may read, by its `paths` and
  `allowUserFileRetrieve`, and the albums, artists and genres of those. A
  library it can read nothing of is not shown to it.
- **The folder needs an account,** over HTTP and WebDAV, even where a pattern of
  `pathsWithoutAuth` matches it. TFTP knows no accounts and serves
  it like any other folder, so keep TFTP off, or its folder elsewhere, where
  the music is private.
- **What an account plays is kept** in the
  [persistent data file](INSTALL.md#run), across clients and restarts. A track
  counts as played once half of it, or four minutes, was heard, as Last.fm
  counts a scrobble, and each play goes into the account's history. Albums,
  artists, genres and tracks can be favorites. A track of ten minutes or more,
  a mix or a chapter, is resumed where it was stopped. The views a client sets
  are kept per client. Playlists are not kept: a client sees none.

The record of a library on the REPOSITORY tab shows how many tracks and
albums it has, and the address clients add. A binary built with the tag
`no_jellyfin`, and go-fs-light, have no music library.
