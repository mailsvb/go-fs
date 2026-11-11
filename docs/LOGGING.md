# Logging and diagnostics

Everything the program has to say goes to **standard output**, one record per
line, so whatever runs it — a terminal, systemd, a container — collects the log
the way it collects any other program's. With `-logfile`, which the Windows
service uses since it has no standard output, go-fs writes the log to a file
itself, rotated at 10 MiB with five old files kept; started from a command
line rather than by the service manager, it writes to standard output as well,
so the terminal sees what the file gets. Errors that stop it from starting at
all, a configuration that does not parse say, go to standard error before any
logger exists, and to that file.

```toml
[general]
logLevel = "info"    # debug, info, warn, error
logFormat = "text"   # text or json
```

`logFormat = "json"` writes one JSON object per line for a log collector;
`text` is `key=value` for a person. `logLevel` can be **changed without a
restart**: edit the file, and the watcher applies the new level within
`reloadInterval` seconds (`kill -HUP` applies it at once). That is the way to
look at a problem on a running server — switch to `debug`, reproduce, switch
back. `logFormat` needs a restart, and a reload that changes it says so. (An
older file's `[log]` section with `level` and `format` is ignored, and the
server says so at startup.)

What each level holds:

| Level | What is written |
|---|---|
| `error` | a server that cannot start or reload, a request handler that panicked, a file operation that failed on the server's side |
| `warn` | a generated (temporary) certificate, host key or session secret; a certificate that has expired or expires within 30 days; a configuration key this version no longer reads; a password out of the documentation; a request from another origin; an FTP passive port that cannot be opened; an SSH algorithm with known weaknesses listed in `general.ssh` |
| `info` | startup with the version, Go version, platform, PID, host, working folder, the account go-fs runs as, the CPUs and the configuration path; every listener; every reload and what it changed; every login, logoff, refused login and refused connection; every download, upload, delete, mkdir, rename and chmod, with the account, the file, the byte count and how long it took; every transfer that failed and why; every TFTP request that was refused and why; shutdown with the signal that caused it |
| `debug` | the trace of everything go-fs does, below: what goes in and out of every interface, every file it touches, every program it runs and the system calls that matter, and the reason behind every refusal. Every debug record carries `source=file.go:line`, which says where in the program it was written |

Records are structured: a message and `key=value` attributes. The attributes
are consistent across the servers, so `grep user=alice` or `jq 'select(.user
== "alice")'` finds everything one account did whichever protocol it used, and
`client=` on a connection scoped record ties a session's trace together:

```
time=2026-09-17T14:32:01.123+02:00 level=INFO msg="ftp login" client=10.0.0.5:51234 user=alice address=10.0.0.5 total=1
time=2026-09-17T14:32:04.456+02:00 level=INFO msg="ftp upload" client=10.0.0.5:51234 user=alice file=/in/report.pdf bytes=182044 address=10.0.0.5 took=312ms
time=2026-09-17T14:32:09.789+02:00 level=INFO msg="ftp data connection failed" client=10.0.0.5:51234 user=alice mode=passive timeout=30s error="the data connection was not established"
```

Things worth knowing when reading a log:

- **A refused password is `info`**, under `ftp login refused`, `sftp login
  refused` and `http login refused`, with the account name and the address it
  came from. The reason — no such account, wrong password, an account that
  only has keys — is only in the `debug` trace, since the client is never told
  either. An SSH key that is not authorized is `debug` too, with its
  fingerprint, because a client offers every key it has before the right one;
  a client that runs out of keys and gives up shows as `sftp connection closed
  by authenticating client`.
- **`ftp data connection failed`** is nearly always a firewall or a NAT between
  the two ends. `mode=passive` means the client could not reach a port in
  `passiveMinPort`–`passiveMaxPort`; `mode=active` means the server could not
  reach the client. See [The FTP data channel](PROTOCOLS.md#the-ftp-data-channel).
- **`tftp request refused`** names the file and the reason, since TFTP has no
  login and a client that "cannot get the file" is otherwise invisible.
  `tftp transfer failed` says how far a transfer got and why it stopped.
- **`http >`** at `debug` is the access log: method, path, status, bytes
  and duration for every request, including the ones that never reached a
  handler.
- **`the configuration file changed but says the same thing`** is what a save
  that touched nothing looks like; `applying the changed configuration
  sections=ftp,log` names what did change.
- The SFTP host key fingerprint and the TLS certificate's subject, names and
  expiry are logged at startup, so a client's complaint about either can be
  checked against what the server actually serves.

## The debug trace

At `debug` every record of a server names it with `service=` (`http`, `ftp`,
`sftp`, `tftp`, `nbd`, `repositories`, `syncs`), and what crosses an
interface is a pair of records, `<` for what came in and `>` for what went
out:

| Records | What they hold |
|---|---|
| `http <`, `http >` | every request: method, URL, host, protocol, the whole header, TLS version, cipher, server name and ALPN; the answer: status, header, bytes sent, bytes of the body read, duration. `req=` numbers the request, and every record written while it is served carries the same number, the files it touched among them |
| `ftp <`, `ftp >` | every command and reply, AUTH and an ABOR during a transfer included; `conn open` and `conn closed` for each data connection, with the bytes in each direction |
| `sftp <`, `sftp >`, `ssh <` | every request and how it ended; the SSH handshake with both versions and the key exchange, cipher and MAC agreed; global requests such as keepalives |
| `tftp <`, `tftp >` | every request, OACK and ERROR with its options; of DATA and ACK the first block, the last, every retransmission and every duplicate ACK, which is what tells how a transfer went without a line per block |
| `nbd <`, `nbd >` | every option and reply of the handshake; every request with its command, cookie, offset and length, and its answer with the error and the duration |
| `file <op>` | every file go-fs opens, creates, lists, reads, writes, renames, removes or changes, with the path, what the call knows for free (size, mode, flags, bytes, entries, the target of a rename) and the error |
| `store <op>` | every read and write of the persistent data file, with the bucket, the key and the size |
| `exec start`, `exec exit` | every program go-fs runs (yt-dlp, ffmpeg, the probes of `general.tools`, the check of a downloaded update): path, arguments, PID, how it ended, the time and the CPU it took; `yt-dlp >` and `ffmpeg >` are the lines they print |
| `syscall <name>` | holes punched and ranges zeroed in disk images, extents asked for, fsync, sparse copies, free space, the exec into an update, the signals subscribed to |
| `out http >`, `out http <` | every request go-fs sends: to another registry, a URL fetched, Artifactory, a Sonos speaker, GitHub for an update; `peer=` names which |
| `remote <op>` | every login to a stored server, the unlocking of a vault, and every operation on it, on the host and in the vault, with the SSH handshake or the SMB session under it |
| `ssdp <`, `ssdp >` | the search for Sonos speakers on every interface and every answer |
| `service <`, `service >` | what the Windows service manager asks and is told |

What authenticates is never written, at any level: a password, the
OpenSubsonic `p`, `t`, `s` and `apiKey`, a Jellyfin `api_key` or token, the
key of a share link, an S3 signature, the `Authorization` header (only its
scheme is kept), cookies (only their names are kept) and the arguments a
program is given to log in with are shown as `***`, in URLs, headers and the
lines a program prints alike. Bodies are never written, only their size and
type, and neither is what a disk image or a file holds. The configuration in
effect is written once at startup as `configuration in effect`, with every
key that holds a credential masked.

The trace is large: a busy server writes several records per request, and a
listing of a big folder one per file it looks at. It is meant to be switched
on to look at a problem and off again, which a reload does without a restart.

A crash prints Go's panic and stack trace to standard error; a panic inside an
HTTP request handler is recovered by the server and logged at `error` with the
stack trace, so the request fails but the server keeps running.
