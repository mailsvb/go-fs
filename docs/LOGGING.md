# Logging and diagnostics

Everything the program has to say goes to **standard output**, one record per
line, so whatever runs it — a terminal, systemd, a container — collects the log
the way it collects any other program's. Only with `-logfile`, which the
Windows service uses since it has no standard output, does go-fs write the log
to a file itself, rotated at 10 MiB with five old files kept. Errors that stop
it from starting at all, a configuration that does not parse say, go to
standard error before any logger exists, or to that file.

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
| `info` | startup with the version, Go version, platform, PID and configuration path; every listener; every reload and what it changed; every login, logoff, refused login and refused connection; every download, upload, delete, mkdir, rename and chmod, with the account, the file, the byte count and how long it took; every transfer that failed and why; every TFTP request that was refused and why; shutdown with the signal that caused it |
| `debug` | the protocol trace: every FTP command and reply (the password masked), every SFTP request, every HTTP request and response with status, size and duration, every TFTP packet exchange, every connection as it comes and goes, and the reason behind every refusal. Every debug record carries `source=file.go:line`, which says where in the program it was written |

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
- **`http response`** at `debug` is the access log: method, path, status, bytes
  and duration for every request, including the ones that never reached a
  handler.
- **`the configuration file changed but says the same thing`** is what a save
  that touched nothing looks like; `applying the changed configuration
  sections=ftp,log` names what did change.
- The SFTP host key fingerprint and the TLS certificate's subject, names and
  expiry are logged at startup, so a client's complaint about either can be
  checked against what the server actually serves.

A crash prints Go's panic and stack trace to standard error; a panic inside an
HTTP request handler is recovered by the server and logged at `error` with the
stack trace, so the request fails but the server keeps running.
