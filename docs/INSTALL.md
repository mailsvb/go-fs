# Install and run

Installing go-fs as a service, running it by hand, and updating it.

## Install

Every release carries an rpm for x86_64 and aarch64 and an msi for x64, besides
the `.update` files and `SHA256SUMS`. They install go-fs
as a service that starts with the system, with a configuration that serves HTTP
on `127.0.0.1:8080` alone, with the [container registry](REGISTRY.md#container-registry) on,
and has one account, `admin` with the password `admin`, which may also push to
the registry. Log in with it at `http://127.0.0.1:8080/?go-fs=admin` from the
machine itself, change its password, and then set up the servers and the
addresses they listen on. go-fs warns about that password at every start until
it is changed. The rpm prints that address when it is installed or updated, and
the msi offers to open it in the browser on its last page.

An rpm install is updated through the package repository, which keeps the
configuration as it is; [Updating over HTTP](#updating-over-http) refuses to
replace a binary the rpm installed. An msi install updates itself from the
GitHub releases, see [Windows: msi](#windows-msi).

Every release is also a container image for linux/amd64,
`ghcr.io/mailsvb/go-fs`, see [Linux: Docker](#linux-docker).

### Linux: rpm

The rpms are signed, and served as a zypper and dnf repository from GitHub
Pages, which keeps the five newest releases:
<https://mailsvb.github.io/go-fs/>.

```shell
# openSUSE and SLES
zypper addrepo https://mailsvb.github.io/go-fs/rpm/go-fs.repo
zypper refresh && zypper install go-fs
zypper update go-fs                            # an update

# Fedora, RHEL and Rocky Linux
dnf config-manager addrepo --from-repofile=https://mailsvb.github.io/go-fs/rpm/go-fs.repo
dnf install go-fs

systemctl status go-fs
journalctl -u go-fs -f                         # the log
zypper remove go-fs                            # or dnf remove go-fs
```

The first `zypper refresh` asks to trust the repository's key, `go-fs.asc` on
the same page. A downloaded rpm of a release installs too, with
`zypper install ./go-fs-<version>-1.x86_64.rpm` or `.aarch64.rpm`.

| Path | What |
|---|---|
| `/usr/bin/go-fs` | the binary |
| `/etc/go-fs/go-fs.toml` | the configuration, owned by the `go-fs` account, mode `0600` |
| `/var/lib/go-fs/files` | the folder the configuration serves |
| `/var/lib/go-fs/registry` | the folder of the container registry |
| `/var/lib/go-fs/trash` | the trash, what is deleted over HTTP and WebDAV |
| `/usr/lib/systemd/system/go-fs.service` | the unit, which runs go-fs as the `go-fs` account |

The package creates the `go-fs` account, then enables and starts the service. An
update restarts it, and a removal stops and disables it. The configuration, the
served files and the account stay after a removal; the configuration is renamed
to `go-fs.toml.rpmsave`, since go-fs has written its secrets into it by then. An
update leaves a changed
configuration file alone, and puts the one it ships beside it as
`go-fs.toml.rpmnew`.

The unit allows go-fs to bind ports below 1024 without root, so the servers can
move to 21, 22, 69, 80 or 443. It keeps `/usr` and `/etc` read-only, except
`/etc/go-fs`. A folder elsewhere only needs to be readable, or writable, by the
`go-fs` account. Change the unit with a drop-in rather than by editing it:
`systemctl edit go-fs`.

### Linux: Docker

Each release is pushed as `ghcr.io/mailsvb/go-fs:<version>`, and, unless it is
a pre-release, as `:<major>.<minor>` and `:latest`, for linux/amd64 only. It is
Alpine with go-fs and what [Fetch](BROWSER.md) runs: yt-dlp, the ffmpeg and
ffprobe it merges and converts with, and deno, which yt-dlp needs for YouTube.
go-fs finds them by itself, so `general.tools` stays empty. Each image carries
the yt-dlp that was current when it was built; the label
`io.github.mailsvb.go-fs.yt-dlp.version` says which.

```shell
docker volume create go-fs-config
docker run -d --name go-fs --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v go-fs-config:/etc/go-fs \
  -v /srv/media:/var/lib/go-fs/files \
  ghcr.io/mailsvb/go-fs:latest
docker logs -f go-fs                           # the log
```

The configuration is the rpm's, except that every server listens on all
interfaces of the container, since a published port does not reach
`127.0.0.1` inside it. Publish the HTTP port on `127.0.0.1` of the host, as
above, until the password of `admin` is changed at
`http://127.0.0.1:8080/?go-fs=admin`. Arguments after the image name are
passed on to go-fs, such as `-logfile`.

| Path | What |
|---|---|
| `/usr/bin/go-fs` | the binary |
| `/etc/go-fs/go-fs.toml` | the configuration, and `go-fs.db` beside it, a volume |
| `/var/lib/go-fs/files` | the folder the configuration serves |
| `/var/lib/go-fs/registry` | the folder of the container registry |
| `/var/lib/go-fs/trash` | the trash, what is deleted over HTTP and WebDAV |

Both `/etc/go-fs` and `/var/lib/go-fs` are volumes. Mount a folder at
`/etc/go-fs`, never the file alone: go-fs saves the configuration by renaming
a new file over it, which a file mounted on its own refuses. An empty folder
is given the starter configuration at the first start.

go-fs runs as the account `go-fs`, UID and GID 10001, so a folder of the host
mounted into the container must be writable by that UID, for example with
`chown -R 10001:10001 /srv/media`. Or run it as the owner of the host's
folders with `--user 1000:1000`, and mount folders of that owner at both
`/etc/go-fs` and `/var/lib/go-fs`, since those of the image belong to 10001;
go-fs makes the folders of `[general]` it is missing itself.

A server other than HTTP also needs its port published once it is switched
on: `-p 8443:8443`, `-p 8022:8022`, `-p 8069:8069/udp` and so on. Passive FTP
needs the whole range, `-p 1024-1034:1024-1034` by default, and
`ftp.passiveAddress` set to the address of the host, since the container's own
is not one a client can reach. Finding Sonos speakers and announcing the
Jellyfin server use multicast on the local network, which reaches a container
only with `--network host`; then no `-p` is needed.

An image is updated by the image of the newer release, and keeps what is in
its volumes. [Updating over HTTP](#updating-over-http) refuses to replace the
binary of an image, which recreating the container would undo.

```shell
docker pull ghcr.io/mailsvb/go-fs:latest
docker rm -f go-fs && docker run -d --name go-fs ...  # as before
```

### Windows: msi

```powershell
msiexec /i go-fs-<version>-x64.msi /qn
msiexec /i go-fs-<version>-x64.msi /qn FIREWALL=1 # and let go-fs through the firewall
Get-Service go-fs
msiexec /x go-fs-<version>-x64.msi /qn
```

| Path | What |
|---|---|
| `C:\Program Files\go-fs\go-fs.exe` | the binary |
| `C:\ProgramData\go-fs\go-fs.toml` | the configuration |
| `C:\ProgramData\go-fs\files` | the folder the configuration serves |
| `C:\ProgramData\go-fs\registry` | the folder of the container registry |
| `C:\ProgramData\go-fs\trash` | the trash, what is deleted over HTTP and WebDAV |
| `C:\ProgramData\go-fs\logs\go-fs.log` | the log, rotated at 10 MiB, five old files kept |

The service `go-fs` runs as LocalSystem. It starts with Windows and is restarted
if it crashes. `C:\ProgramData\go-fs` is readable by SYSTEM and the
Administrators only, because the configuration holds every password and key. An
uninstall removes the binary and the service, and keeps everything in
`C:\ProgramData\go-fs`. The firewall stays closed unless `FIREWALL=1` is
given, since nothing listens beyond `127.0.0.1` as installed.

Once installed, go-fs updates itself: turn on `http.enableSelfUpdate`, and the
**UPDATE** tab of the admin interface installs the latest GitHub release (see
[Updating from GitHub](#updating-from-github)). go-fs replaces `go-fs.exe`,
keeping the previous one as `go-fs.exe.old`, and ends the service with an
error, which the service manager answers with a restart on the new binary a
few seconds later. The msi of a newer release also updates, run like the first
install. It compares its version with the msi installed, not with what go-fs
updated itself to, so an msi older than the running go-fs puts that older
version back.

The installer is not code signed yet, so Windows SmartScreen warns about the
msi when it is downloaded.

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
| `-logfile <path>` | write the log to this file, rotated at 10 MiB with five old files kept; what the Windows service uses. Started from a command line, go-fs writes to standard output as well |
| `-persistent-data <path>` | file go-fs keeps what it remembers across a restart in, such as share links and playback progress; by default beside the configuration file and named like it, `go-fs.toml` keeping its data in `go-fs.db` |
| `-version` | print the version and exit |

The persistent data file is created at the first start, readable by its
owner only, and held open for as long as go-fs runs: a second go-fs pointed at
the same file refuses to start. An uninstall keeps it, as it keeps the
configuration; back the two up together.

Besides share links and playback progress, the file keeps:

* the SFTP host key and the self-signed HTTPS and FTPS certificates go-fs
  generates when none is configured, and the keys it signs digest nonces with,
  so that clients see the same identity after a restart; that makes the file
  as secret as the configuration;
* the time and outcome of each sync's last run, so that a restart neither
  forgets them nor runs a sync again before it is due, and what each sync both
  ways had in sync, which tells a file removed on one side from one new on the
  other;
* what each repository read of its packages, so that a restart reads only the
  files that changed meanwhile and writes no metadata when nothing did;
* the loudness of each MP3 file measured in the
  [tag editor](BROWSER.md#the-volume), so that it is not decoded again while
  it stays as it is; a measurement not used for 180 days is dropped;
* what each account did in the [Jellyfin music library](REPOSITORIES.md#jellyfin-music-library):
  play counts, favorites, resume positions, the views its clients set, the
  history of every track it played, and the tokens its clients logged out
  with. None of it is pruned: what belongs to a file that moved or an account
  that was removed stays, and is back when they are.

What the file keeps of a sync or a repository goes when that is taken out of
the configuration.

`SIGINT` and `SIGTERM` shut the servers down, dropping open connections and
aborting running transfers. Stopping the Windows service does the same.

Every default port is above 1024, so go-fs runs without privileges. Serving on
21, 22 or 69 needs them: prefer a service manager that grants them, as the
[rpm's unit](#linux-rpm) does, or run on high ports behind a redirect.

## Updating over HTTP

A running go-fs can be updated through its own HTTP or HTTPS port. The upload is
a release's `.update` file: the go-fs binary with an ed25519 signature appended.
go-fs checks the file, replaces its executable, and restarts into the new
version. The configuration file is not touched.

It is off by default. Turn it on with `http.enableSelfUpdate = true`, and allow
at least one client. A go-fs installed by the rpm refuses every update over
HTTP, since its package owns the binary: update the package instead. The
**UPDATE** tab then shows the latest release and how to install it.

- **A bearer token** that sets `allowSelfUpdate = true`, for scripts. The token's
  `paths` do not matter here.
- **An admin's browser session.** The admin interface then shows an **UPDATE**
  tab with the running version, the latest release, and an upload button (see
  [Updating from GitHub](#updating-from-github)).

Basic and Digest credentials are refused, even for an admin account, for the
same reason the admin interface refuses them.

```sh
# what is running, to pick the matching file
curl -H "Authorization: Bearer gofs_…" "https://host:8443/?go-fs=update"
# {"version":"1.1.0","os":"linux","arch":"arm64","executable":"/opt/go-fs/go-fs","pid":812,"keys":["d1db60358969ad82"]}

# upload it; go-fs answers, then restarts
curl -T go-fs_1.2.0_linux_arm64.update -H "Authorization: Bearer gofs_…" \
     "https://host:8443/?go-fs=update"
# {"previous":"1.1.0","restarting":true,"version":"1.2.0"}
```

Before anything changes, the upload has to pass these checks, in this order:

1. **Signature.** The file must be signed with a key built into the *running*
   binary. The keys are compiled in from `internal/selfupdate/keys.txt`, so
   nothing at runtime can add one, including an admin editing the
   configuration. Nothing in the file is run before its signature has been
   checked.
2. **Platform.** The executable header must match the server's own OS and
   architecture.
3. **Smoke test.** The new binary is run with `-version` and must answer as
   go-fs.

| Status | Meaning |
|---|---|
| `202` | accepted; go-fs restarts right after answering |
| `400` | the file carries no signature, e.g. a plain binary instead of the `.update` file |
| `409` | another update is in progress, or the rpm owns the binary |
| `413` | larger than 256 MiB |
| `422` | unknown key, bad signature, wrong platform, or not a go-fs binary |
| `503` | this build trusts no signing key |

The signature is cut off before the binary is installed, so what lands on disk
is byte for byte the release build. On macOS the operating system's own code
signature therefore stays valid.

How the executable is replaced:

- The new binary is written into the folder that holds the executable, so that
  folder has to be writable by the account go-fs runs as.
- The running binary is renamed to `<executable>.old` and kept as the way back.
  The next update replaces it.
- On Linux and macOS go-fs restarts with `exec`: the process keeps its PID,
  arguments and environment, so systemd, launchd, a container or a terminal
  keeps tracking it.
- On Windows go-fs starts the new version as a new process and exits. Whatever
  started the old process sees it end. `pid` in the answer to `GET` shows the
  new process. As a Windows service, go-fs instead ends the service with an
  error, after making sure the service manager restarts it on one, and the
  service manager starts the new version.
- If the new binary cannot be moved into place, the previous one is restored
  and started again, so the servers come back either way.
- Open connections are dropped by the restart, as they are by `SIGTERM`.

Browsers stay logged in across the restart, since `http.httpSessionTokenSecret`
is kept in the configuration file. Only when that file cannot be written, and
the key is made for each run, does the restart log every browser out, the admin
page included.

### Updating from GitHub

go-fs can also fetch the update file itself, from the latest release on
GitHub. When an admin opens the admin interface, it looks up the latest
release in the background. If that release is newer than the running version,
the **UPDATE** tab is marked with its version number and the page says so. The
tab then offers **Update to X and restart**: go-fs downloads that release's
`.update` file for its own platform and installs it exactly as if it had been
uploaded. It goes through the same three checks, so a file that is not signed
with a trusted key is refused wherever it came from.

A script does the same with `release=latest`:

```sh
# the latest release, and whether it is newer than what runs
curl -H "Authorization: Bearer gofs_…" "https://host:8443/?go-fs=update&release=latest"
# {"current":"1.1.0","version":"1.2.0","newer":true,"url":"https://github.com/…/v1.2.0",
#  "published":"2026-10-01T10:00:00Z","asset":"go-fs_1.2.0_linux_arm64.update"}

# download and install it; go-fs answers, then restarts
curl -X POST -H "Authorization: Bearer gofs_…" "https://host:8443/?go-fs=update&release=latest"
# {"previous":"1.1.0","restarting":true,"version":"1.2.0"}
```

- Only the latest release can be installed this way. Drafts and pre-releases
  are skipped. To install another version, upload its file.
- The lookup is kept for 15 minutes, since GitHub answers only 60
  unauthenticated requests an hour. **Check again** on the tab, or
  `&refresh=1`, asks GitHub anew.
- go-fs contacts GitHub only when it is asked to: when the admin interface is
  opened with `http.enableSelfUpdate` on, or when a script asks. Nothing is
  checked at startup or in the background.
- The server needs outbound HTTPS to `api.github.com` and `github.com`, which
  redirects the download to `objects.githubusercontent.com`. `HTTPS_PROXY` and
  `NO_PROXY` are honoured. A server that cannot reach GitHub shows why on the
  tab and can still be updated by upload.
- Besides the statuses above, the endpoint answers `502` when GitHub cannot be
  reached or does not answer as expected, and `404` when the latest release has
  no update file for this server's platform.
- A fork that publishes its own releases sets where they are looked up at build
  time, with
  `-ldflags "-X github.com/mailsvb/go-fs/internal/selfupdate.releaseAPI=https://api.github.com/repos/<owner>/<repo>/releases/latest"`.
  It also needs its own signing key, see below.

**Signing key.** Generate the key pair once:

```sh
go run ./tools/sign -generate
```

The command prints a private key and a public key:

- Store the private key as the `GOFS_SIGNING_KEY` secret of the repository, which
  the release workflow signs with.
- Add the public key as a line of `internal/selfupdate/keys.txt` and commit it.
  Builds from then on trust it.

To sign a build by hand:

```sh
GOFS_SIGNING_KEY=… go run ./tools/sign -in go-fs_linux_arm64 -out go-fs_linux_arm64.update
```

A fork that signs its own builds can add its key at build time instead of
editing `keys.txt`, with
`-ldflags "-X github.com/mailsvb/go-fs/internal/selfupdate.buildKeys=<public key>"`.

To rotate the key:

1. Add the new public key to `keys.txt`.
2. Release a version still signed with the old key, and roll it out.
3. Switch the secret to the new key and remove the old one from `keys.txt`.
