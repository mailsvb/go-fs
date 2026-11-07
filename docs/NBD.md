# VM disks over NBD

go-fs can keep the disks of QEMU and libvirt VMs. The images live in a folder
of their own, `general.baseFolderDisks`. The **Disks page** in the web interface
makes and manages them, and the **NBD server** serves them to QEMU's built-in
`nbd` client. A VM runs on go-fs storage as if the disk were local, and the
host needs no extra software.

- An image is a **sparse raw file**. A new 100 GB disk takes no space until the
  guest writes to it, and space the guest trims goes back to the filesystem.
- Every connection uses **TLS with a client certificate** from a CA of your own.
  The certificate's common name is the go-fs account it logs in as.
- **One VM at a time** may attach a disk read-write. A VM that loses its
  connection and comes back with the same certificate takes over at once.
- A **full filesystem pauses the VM** (`werror=stop`) instead of corrupting its
  disk.

## Set it up

```toml
[general]
baseFolderDisks = "/var/lib/go-fs/disks"

[nbd]
enabled = true
port = 10809
address = ""              # every interface; the default 127.0.0.1 is this host only
cert = "…"                # base64 < server.crt | tr -d "\n"
key = "…"                 # base64 < server.key | tr -d "\n"
clientCA = "…"            # base64 < ca.crt | tr -d "\n"
clientCRL = ""            # optional, base64 < ca.crl | tr -d "\n"
allowedClients = ["192.168.10.0/24"]

[[users]]
username = "kvm01"         # the common name of the host's client certificate
nbd = true
allowUserFileRetrieve = true
allowUserFileOverwrite = true   # leave this out for read-only access
```

- **The disks folder.** It must not be inside, or contain, any other go-fs
  folder (files, registry, temp, trash, an account's base folder). No file
  server, sync or cleanup can then touch a running VM's disk. go-fs creates it
  with mode `0750` at startup.
- **Ports.** The NBD server has its own port, 10809 by default. It cannot share
  the HTTP port: an NBD client waits for the server to speak first, so a shared
  port would have to guess from silence.
- **Changes apply live.** The certificate, the CA, the CRL, accounts and limits
  take effect without a restart. Connections the new settings no longer admit
  are closed at once: a revoked certificate, an account removed or switched to
  `nbd = false`, or a read-write session whose account lost
  `allowUserFileOverwrite`. Changing the port, the address, `keepAlive` or the
  folder restarts the NBD server.

The admin page edits all of this under its **NBD** tab. `cert`, `key`,
`clientCA` and `clientCRL` take an uploaded PEM file there. There is no
Generate button: a certificate go-fs made up would not be trusted by QEMU.

## Certificates

go-fs issues no certificates. QEMU checks the server's certificate against the
CA it is given, and go-fs checks the client's against `clientCA`. One small CA
serves both. With openssl:

```sh
cat > ca.cnf <<'EOF'
[req]
distinguished_name = dn
[dn]
[v3_ca]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
[v3_server]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:gofs.lan
[v3_client]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = clientAuth
EOF

# the CA
openssl req -x509 -new -newkey rsa:3072 -nodes -keyout ca.key -out ca.crt \
  -days 3650 -subj "/CN=go-fs NBD CA" -config ca.cnf -extensions v3_ca

# the server: subjectAltName above has to be the name QEMU connects to
openssl req -new -newkey rsa:3072 -nodes -keyout server.key -out server.csr \
  -subj "/CN=gofs.lan" -config ca.cnf
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 825 -extfile ca.cnf -extensions v3_server

# a client: the common name is the go-fs account
openssl req -new -newkey rsa:3072 -nodes -keyout kvm01.key -out kvm01.csr \
  -subj "/CN=kvm01" -config ca.cnf
openssl x509 -req -in kvm01.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out kvm01.crt -days 825 -extfile ca.cnf -extensions v3_client
```

`server.crt`, `server.key` and `ca.crt` go into `[nbd]`. On the VM host, QEMU
reads three files from one folder, `/etc/pki/qemu` by convention:

| File | Holds |
|---|---|
| `ca-cert.pem` | `ca.crt` |
| `client-cert.pem` | `kvm01.crt` |
| `client-key.pem` | `kvm01.key`, readable by the user QEMU runs as |

### Revoking a certificate

A certificate stops working when it expires, or when its account is removed or
loses `nbd = true`. To revoke a single certificate before it expires, publish
a CRL:

```sh
cat > crl.cnf <<'EOF'
[ca]
default_ca = local
[local]
database = index.txt
certificate = ca.crt
private_key = ca.key
default_md = sha256
default_crl_days = 30
EOF
touch index.txt

openssl ca -config crl.cnf -revoke kvm01.crt
openssl ca -config crl.cnf -gencrl -out ca.crl
base64 < ca.crl | tr -d "\n"      # → nbd.clientCRL
```

Version 1 CRLs, which plain `openssl ca -gencrl` writes, are accepted, and so
are version 2 ones. A connection that logged in with a revoked certificate is
closed within `general.reloadInterval` seconds of saving the file. Every later
attempt is refused, and the log says why. go-fs warns at startup once the CRL
is past its `nextUpdate`.

## Attach a disk with QEMU

```sh
qemu-system-x86_64 … \
  -object tls-creds-x509,id=tls0,endpoint=client,dir=/etc/pki/qemu \
  -blockdev driver=nbd,node-name=d0-nbd,server.type=inet,server.host=gofs.lan,server.port=10809,export=web01.raw,tls-creds=tls0,reconnect-delay=10 \
  -blockdev driver=raw,node-name=d0,file=d0-nbd,discard=unmap,detect-zeroes=unmap \
  -device virtio-blk-pci,drive=d0,werror=stop,rerror=stop
```

| Option | What it does here |
|---|---|
| `export=web01.raw` | The disk's name on the Disks page. |
| `server.host` | Has to match the server certificate's subjectAltName. |
| `reconnect-delay=10` | The VM keeps running through a short network drop. When it comes back with the same certificate, it takes over its disk from the dead connection. |
| `discard=unmap` | The guest's TRIM punches holes in the image, so the space goes back to the filesystem. Run `fstrim` in the guest, or mount with `discard`. |
| `detect-zeroes=unmap` | Writes of zeros become holes too. |
| `werror=stop,rerror=stop` | When the go-fs filesystem is full, QEMU gets `ENOSPC` and pauses the VM. Make space, then `cont` in the monitor. |

The Disks page's **Connection details** fills these lines in for a disk, with
this server's name and port.

### libvirt

In `/etc/libvirt/qemu.conf`:

```
nbd_tls = 1
nbd_tls_x509_cert_dir = "/etc/pki/qemu"
```

and in the domain:

```xml
<disk type='network' device='disk'>
  <driver name='qemu' type='raw' discard='unmap' detect_zeroes='unmap' error_policy='stop'/>
  <source protocol='nbd' name='web01.raw' tls='yes'>
    <host name='gofs.lan' port='10809'/>
  </source>
  <target dev='vda' bus='virtio'/>
</disk>
```

### Without a VM

`qemu-img` and `qemu-io` speak NBD too, with the same options:

```sh
TLS="--object tls-creds-x509,id=tls0,endpoint=client,dir=/etc/pki/qemu"
DISK="driver=nbd,server.type=inet,server.host=gofs.lan,server.port=10809,export=web01.raw,tls-creds=tls0"

qemu-img info $TLS --image-opts "$DISK"
qemu-img map --output=json $TLS --image-opts "$DISK"          # which parts hold data
qemu-img convert -p -O raw $TLS --image-opts "$DISK" web01.raw # a sparse local copy
```

To put an existing qcow2 image on go-fs, convert it to raw first
(`qemu-img convert -O raw disk.qcow2 disk.raw`). Then upload it on the Disks
page, which leaves its blocks of zeros as holes.

## The Disks page

Accounts with `isAdmin` see **Disks** in the menu, after Files and Registry.
The page lists every image with its size, the space it takes, who has it
attached and when it last changed. It offers:

- **New disk image**: a name and a size such as `20G` or `1.5T`. The new image
  is sparse and empty.
- **Upload disk**: a raw image from your computer, in resumable pieces of
  `http.maxChunkSize`. Blocks of zeros in it are left as holes.
- **Connection details**: QEMU and libvirt lines for the disk.
- **Resize**: refused while anything has the disk attached. Making a disk
  smaller has to be asked for, since it cuts off whatever the guest wrote at
  its end. The guest grows its partition and filesystem itself afterwards.
- **Rename**: refused while attached. Change the VM's `export=` as well.
- **Clone**: a copy as sparse as the original, made in the background with its
  progress shown. Where the filesystem can share blocks (btrfs, XFS, APFS) it
  takes no time and no space. While it runs, the original can still be read
  but not attached read-write.
- **Download**: the image whole. Refused while it is attached read-write,
  because it would change during the download.
- **Delete**: for good, after typing the disk's name. A deleted disk does not
  go to the trash.
- **Disconnect**: closes one client's connection. A VM with `reconnect-delay`
  connects again, so to stop it for good, revoke its certificate or switch off
  its account first.

## Who may do what

| Already on the disk | NBD read-write | NBD read-only | Download, clone source | Resize, rename, delete, replace |
|---|---|---|---|---|
| nothing | yes | yes | yes | yes |
| a read-write client | only the same certificate, which takes over | yes | no | no |
| read-only clients | yes | yes | yes | no |
| a download or clone | no | yes | yes | no |

An account attaches read-write when it has `allowUserFileOverwrite`, and
read-only otherwise. The NBD protocol does not tell the server that a client
opened the disk read-only, so a VM given `read-only=on` still holds the write
lock if its account may write. Give readers an account of their own without
`allowUserFileOverwrite`. Read-only clients may attach any number of times,
alongside a writer.

A connection that dies without closing keeps its lock until TCP keepalive
notices: about `nbd.keepAlive` seconds plus fifteen. A reconnect with the same
certificate does not wait for that.

## Space

A disk takes only the space its data needs, as long as the guest trims. On
Linux (ext4, XFS, btrfs, tmpfs) and Windows (NTFS, ReFS), TRIM and writes of
zeros punch real holes, and `qemu-img map` reports them. macOS keeps a trimmed
range allocated. It serves disks just as well, but gives no space back.

An NBD disk's size is fixed while it is attached. That is why the image is raw
rather than qcow2: a qcow2 image grows inside a fixed export and runs out of
room.

## Limits and errors

- A request may be up to 32 MiB, and up to sixteen run at once per connection.
- QEMU 9 and later ask for extended headers first. go-fs declines, and QEMU
  goes on with structured replies, which is what `qemu-img map` and
  `convert` use.
- What QEMU reports, and what it means:

| QEMU says | go-fs log says | Meaning |
|---|---|---|
| `Requested export not available` / `there is no disk "…"` | — | The export name is not a disk on the Disks page. |
| `Denied by server for option 7 (go)` / `"…" is attached read-write by …` | `nbd attach refused` | Another client holds the disk read-write. |
| `Could not open image: Permission denied` | `nbd attach … mode=read-only` | The account may only read; add `read-only=on`, or grant `allowUserFileOverwrite`. |
| `A TLS fatal alert has been received` | `nbd login refused` with the reason | The certificate is unknown, expired, revoked, or names no account that sets `nbd`. |
| the VM pauses with an I/O error | `nbd disk full` | The filesystem of the disks folder is full. |
