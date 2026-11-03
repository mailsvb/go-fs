# go-fs

**Every file protocol, one folder, one binary.**

go-fs serves one folder over FTP and FTPS, SFTP, HTTP and HTTPS, WebDAV, an
S3 compatible API and TFTP, with one list of accounts across all of them. It
is a single statically linked Go binary, configured from one TOML file that
reloads without dropping a connection, and edited from a web interface the
HTTP server serves to its admins.

## Why go-fs

- **One tree, every client.** FTP, SFTP, HTTP, WebDAV, S3 and TFTP all see the
  same files. An account is set up once, with one password, and lists the
  protocols it may use.
- **Change it live.** Edit the file or use the admin page. Accounts, rights
  and limits change on the running servers, and a download in progress
  finishes.
- **More than a file server.** An OCI container registry, rpm and F-Droid
  repositories that update themselves, and a music library that Jellyfin
  clients can play from.
- **A real browser UI.** Share links, a trash, folder downloads, fetch from a
  URL, send to SFTP, SMB or Artifactory (in a Cryptomator vault if you like),
  and scheduled two-way sync.
- **Safe by default.** Listens on `127.0.0.1` only, generates its own
  certificates and host key, offers bearer tokens for scripts, and only
  installs updates that carry a valid signature.

## Get it

Every [release](https://github.com/mailsvb/go-fs/releases) has an rpm for
x86_64 and aarch64 and an msi for Windows x64. Both install go-fs as a
service ([details](docs/INSTALL.md)). On openSUSE and SLES (Fedora and RHEL:
see the [package page](https://mailsvb.github.io/go-fs/)):

```sh
sudo zypper addrepo https://mailsvb.github.io/go-fs/rpm/go-fs.repo
sudo zypper refresh && sudo zypper install go-fs
# → http://127.0.0.1:8080/?go-fs=admin, log in as admin / admin
```

On Windows:

```powershell
msiexec /i go-fs-<version>-x64.msi   # offers to open the admin page when done
```

## Run it

```sh
go-fs -init go-fs.toml     # write the documented starter configuration
$EDITOR go-fs.toml         # set the folders and the accounts
go-fs -check               # report problems in it
go-fs                      # → http://127.0.0.1:8080/?go-fs=admin
```

The starter configuration serves HTTP alone, on `127.0.0.1:8080`, from
folders under `/var/lib/go-fs`. Point them at a folder the account running
go-fs may write to, then switch on more servers and add accounts in the file
or on the admin page. Every key is explained in
[`go-fs.example.toml`](go-fs.example.toml).

> **Security note:** the starter configuration has an account `admin` with the
> password `admin`, and go-fs warns about it at every start until it is
> changed. The configuration file holds every password and key, so keep it
> `chmod 600`.

## Learn more

| Document | Covers |
| --- | --- |
| [Install and run](docs/INSTALL.md) | rpm, msi, command line flags, persistent data, self-update |
| [Configuration](docs/CONFIGURATION.md) | folders, reloading, accounts, admin interface, defaults, TLS |
| [Protocols](docs/PROTOCOLS.md) | FTP, SFTP, HTTP, WebDAV, bearer tokens, S3, TFTP, supported standards |
| [Container registry](docs/REGISTRY.md) | pushing and pulling images, multi-platform tags |
| [Repositories](docs/REPOSITORIES.md) | rpm and F-Droid repositories, Jellyfin music library |
| [Browser](docs/BROWSER.md) | browser login, trash, folder download, share links, fetch from URL |
| [Remote servers](docs/REMOTE-SERVERS.md) | send to SFTP, SMB or Artifactory, Cryptomator vaults, sync |
| [Logging](docs/LOGGING.md) | log levels and format, reading the log |
| [Development](docs/DEVELOPMENT.md) | building, versioning, releases, go-fs-light |

## License

[Apache 2.0](LICENSE).
