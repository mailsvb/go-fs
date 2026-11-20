# Container registry

The HTTP server can also be a **container registry**: docker, podman, buildx,
crane and every other client of the
[OCI Distribution Specification](https://github.com/opencontainers/distribution-spec)
v1.1 push images to it and pull them from it, on its plain port and its TLS
port alike. It is off until `general.baseFolderRegistry` names the folder the
registry keeps its images in; each account that should push sets
`registry = true`:

```toml
[general]
baseFolderRegistry = "/var/lib/go-fs/registry"

[http.main]
enabled = true

[http.services]
registryAnonymousRead = true

[[users]]
username = "ci"
password = "…"
registry = true
```

```shell
docker login host:8443
docker tag app:latest host:8443/team/app:1.0
docker push host:8443/team/app:1.0
docker pull host:8443/team/app:1.0
```

| | |
|---|---|
| Endpoint | `host:8443` (or the plain port), under `/v2/` |
| Storage | `general.baseFolderRegistry`, which nothing else may serve |
| Pulling | anyone while `http.services.registryAnonymousRead = true` (the default), otherwise an account that sets `registry` or a `[[tokens]]` entry that sets `allowRegistry` |
| Pushing, tagging, deleting | an account that sets `registry`, or a token that sets `allowRegistry` |
| Credentials | a token from `/v2/_token`, fetched with the account's username and password as `docker login` does, or HTTP Basic on every request; a go-fs bearer token goes where the password goes, or as `Authorization: Bearer` on every request |

The folder is made when it is not there, and it must be none of the folders the servers serve,
nor hold one or lie inside one: go-fs refuses to start with it otherwise, and
`-check` says why. Everything in it is managed by the registry, which addresses
content by its digest alone, so a layer that several images, architectures or
repositories share is stored once. While the registry is on it answers every path under `/v2`, so a folder
`v2` in `general.baseFolderFiles` cannot be reached over HTTP; go-fs says so at warning
level. Both settings take effect on a reload.

`registry` is independent of `http`: an account that sets only `registry`
cannot reach the file tree, and an `http` account that does not set it may
pull where pulling is public and is refused with `403` where it is not.

Clients log in the way every public registry has them do: `/v2/` answers
`401` with a `Bearer` challenge that names `/v2/_token`, the client fetches a
token there, with the username and password it was given by `docker login` or
without any, and sends the token with every request after. A token without an
account pulls where pulling is public; a token of an account that sets
`registry` does everything. Tokens are signed with the key of the browser
sessions, `http.auth.httpSessionTokenSecret`, but cannot stand in for one, and are
accepted for five minutes; changing the account's password ends them at once.
A request may also carry Basic credentials itself, which is what `curl -u`
sends. A wrong password counts against the address in the same lock as
everywhere else, so `docker login` with a wrong one fails as it should.

A go-fs bearer token (`[[tokens]]`, made on the **TOKENS** tab) that sets
`allowRegistry = true` stands in for such an account, which is what CI wants:
`docker login host:8443 -u ci -p gofs_…` takes it as the password, with any
username, and a script may send it directly, as `curl -H "Authorization:
Bearer gofs_…" https://host:8443/v2/_catalog`. Its `paths` and file rights do
not apply here. A token without `allowRegistry` is treated like an account
without `registry`: it pulls where anyone may and is refused with `403`
elsewhere. Replacing the token, letting it expire or turning `allowRegistry` off
ends the registry tokens it was given at once. An unknown token counts against
the address; an expired one does not.

**Several architectures under one tag.** A tag names either an image for one
platform or an index that lists one image per platform, which is what
`docker buildx build --platform linux/amd64,linux/arm64 --push` pushes and what
a pull picks the right image from. go-fs also builds that index itself: an
image pushed to a tag that holds an image for another platform joins it in an
index instead of replacing it, so images built on an amd64 and an arm64
machine and pushed separately end up under one tag:

```shell
# on an amd64 machine
docker push host:8443/team/app:1.0
# on an arm64 machine
docker push host:8443/team/app:1.0
docker manifest inspect host:8443/team/app:1.0   # lists linux/amd64 and linux/arm64
```

The platform comes from the image's configuration. An image for a platform
the tag already holds replaces that entry and the attestations buildx attached
to it; an index that is pushed, and an artifact that is not an image, replace
the tag as they are. The push is answered with the digest of what was pushed,
and the tag then names the index, which the registry keeps only for as long as
a tag points to it.

**Browsing the registry.** While the registry is on, the listing has a
**Registry** button next to **Log in**. It opens a page that looks like the
listing and has its own **Files** button to go back. By default the page lists
every tag on a row of its own, such as `my-company/tool:1.2.3`. **Folders**
shows namespaces and repositories as folders instead, and opening a
repository shows its tags. Each tag carries a badge for every architecture it
holds (`amd64`, `arm64`, `arm/v7`; the OS is only shown when it is not Linux),
and the columns can be sorted by name, push time and size. The options menu
of a tag offers:

- **Details:** digests, media types, push time, sizes, the configuration and
  layers of each platform, and the other tags on the same image.
- **Copy pull command.**
- **Push to registry…:** copies the tag to another registry (see below).
- **Delete tag:** only for a session of an account that sets `registry`.

**Copying images between registries.** With a session of an account that
sets `registry`, the page can also copy images to and from other registries.
Each dialog takes the image (name and tag) and an optional login, and has to
be **validated** before anything is copied: Validate reaches the other
registry with that login and lists the architectures there are to copy, all
ticked. Untick the ones to leave out, then press Pull or Push. Changing the
image or the login takes the validation back. With every architecture ticked
the image is copied as it is and keeps its digest; with some left out, only
those that are ticked (and their attestations) are copied, into an index of
their own with a digest of its own. The server does the copying itself, in
the background: the dialog shows how many blobs and bytes have been moved, can
stop the transfer, and may be closed while it runs; the banner then says how
it ended.

- **Pull image** (top right) fetches an image such as
  `docker.io/library/nginx:1.27`, `ghcr.io/org/app:v1` or
  `registry.example.com:5000/team/tool:v2@sha256:…` into this registry.
  References are read as docker reads them: `nginx` is
  `docker.io/library/nginx:latest`. The image is stored under the path of its
  reference without the registry, `library/nginx:1.27` and `org/app:v1` here,
  and replaces what that tag held. Validate lists the architectures the image
  offers, and the pull fetches the very image that was validated, even if the
  tag moves in between.
- Pull image can instead take its image **From** a container registry stored
  as a server of `type = "registry"` (see
  [REMOTE-SERVERS.md](REMOTE-SERVERS.md#container-registries)). The menu
  lists the registries that admit the account and allow downloads. The dialog
  then browses the registry: its repositories as folders, from the catalog,
  and the tags of each. A registry that lists no catalog, such as Docker Hub,
  is browsed by a repository named in the box below the list. Clicking a tag
  chooses it, and Validate and Pull go on as for an image typed in. The login
  is the one the server stores, Basic, Bearer or a client certificate. The
  page never sees it. The image is stored under its repository and tag, as
  they are on that registry.
- **Push to registry…** copies a tag to the image named, prefilled with the
  tag's own name: put the registry in front, `ghcr.io/my-org/team/app:1.0`,
  or change the name and tag as well. Validate proves that the login may push
  there, says whether the tag is there already, and lists the architectures
  of the tag here. Blobs the other registry already has are not sent again.

Both take an optional username and password (or access token) for the other
registry; they are used for that one transfer, never stored and never logged.
Pull image may send a **Bearer token** instead: it goes as
`Authorization: Bearer <token>`. A registry that turns it down with a
challenge has it exchanged at its token service, as the password of the
username where one is given.
The other registry is reached over HTTPS unless the address starts with
`http://`, as it may for a test registry on `localhost`. **Validate
Connection**, ticked by default, checks the other registry's TLS certificate
as any client would; untick it to accept whatever certificate it shows, a
self-signed one or one of an internal CA (`"skipVerify": true` in the body of
the endpoints, which validate when it is left out). Changing it takes the
validation back like the login does. Blobs are checked
against their digests, and `maxUploadSize` applies to each as it does to a
push. Note that the server connects to whatever host is typed in, from its own
network: give `registry` only to accounts that may do that.

Signatures, attestations and SBOMs go along with the image, so they can still
be checked once it has moved. A pull asks the other registry for the
referrers of the image and of each of its architectures; where it has no
referrers API, it reads the index a client keeps under the tag
`sha256-<hex>` instead. Either way it also looks for cosign's tags
`sha256-<hex>.sig`, `.att` and `.sbom`, and keeps those tags. The same is done
for what it finds, for a signature on an SBOM, and no further. A push sends
them after the image: as referrers, plus cosign's tags only where they are the
one link to a manifest or the other registry has no referrers API (its answer
to a manifest with a `subject` lacks `OCI-Subject`); there it also adds what
has no such tag to the index under `sha256-<hex>`. When only some
architectures are copied, the signatures of the original index stay behind,
as they do not hold for the index of its own. The dialog counts their blobs
with the image's, and the banner says how many went along.

**Importing image archives.** **Import** (top right) stores the images of an
archive that a tool wrote to a file instead of pushing them, so no docker
daemon is needed in between. Pick the file there and it is uploaded in chunks
of `maxChunkSize` (one piece when that is `0`), or use **Import into
registry…** in the options menu of an archive in the listing, which reads that
file where it is; for this the account also needs the right to download that
file. The archive may be a plain tar or compressed with gzip, zstd, xz or
bzip2, and is in one of two formats:

| Format | Written by | Stored as |
|---|---|---|
| OCI image layout (`oci-layout`, `index.json`, `blobs/`) | `docker save` 25 and later, `docker save` with the containerd image store, `ctr images export` (the tool to use on a Kubernetes node, as `crictl` has no export), `nerdctl save`, `podman save --format oci-archive`, `skopeo copy … oci-archive:`, `buildah push … oci-archive:`, a tar of a `crane pull --format oci` folder | as it is: manifests and indexes keep their digests, signatures stay valid |
| `docker save` (`manifest.json`) | `docker save` before 25, `podman save` (`docker-archive`, also `-m`), `skopeo copy … docker-archive:`, `crane pull` (`tarball` and `legacy`), k3s and RKE2 airgap bundles | with a manifest the registry builds, since the archive has none: the image gets a digest of its own, and layers are kept as the archive has them, uncompressed for `docker save` |

The server reads the archive once, checking every blob against its digest,
and then lists the images it found: their names, the architectures of each,
and the `repository:tag` each would be stored under, which is the name the
archive gives without the registry, as a pull names it
(`docker.io/library/alpine:3.20` becomes `library/alpine:3.20`). A tag alone,
as `skopeo` writes it, goes with the archive's file name. Change the targets,
add tags, untick images or architectures, then press Import. An index whose
architectures the archive holds only some of, as `ctr images export` leaves
it without `--all-platforms`, is stored with those, in an index of its own.
A manifest of an OCI layout whose `subject` is one of the images in it,
or that is named only by a cosign tag beside one, as `oras copy -r` and
`cosign save` write a layout, is not offered on its own but stored with that
image, as a referrer and under its tag; one whose image is not in the archive
is offered on its own, and is a referrer once stored all the same.
Up to 30 minutes are given for that choice before what was read is let go.
Nothing is stored until it is made, and the blobs and manifests then go in
together, so a cleanup in between never takes half an image. What is not an
image archive is named as such: the file system of a container
(`docker export`, `crane export`), a zip, a Singularity image, a `dir:` copy of
skopeo, a save of a Docker older than 1.10. `maxUploadSize` applies to the
uploaded archive and to each file in it. The uploaded archive and what was
read are kept under `baseFolderRegistry/_uploads` while the import runs and
removed when it ends.

Who may see the page follows who may pull. With
`http.services.registryAnonymousRead = false` a visitor is sent to the login page
first. The form accepts accounts that set only `registry` wherever it is
reached, also on a server where no account sets `http`. Such an account lands
on the registry page after logging in, and its session reaches nothing in the
file tree: a browser that asks for a file or a folder, public ones included,
is sent to the registry page instead, and its menu holds nothing but **Log
out**.

A server that is there for its registry alone sets
`http.services.files = false`. The http server then answers nothing of
`general.baseFolderFiles`, no listing, no file, no WebDAV, S3 or share link, and
the page at `/` offers the **Registry** and, where an account sets `isAdmin`,
**Admin** instead of **Files**, to a visitor and a session alike. ftp, sftp and
tftp keep their own switches.

What is served:

- **Pulling.** Manifests by tag or digest, blobs with ranges, the tag list and
  the `_catalog` of repositories, both paged with `n` and `last`, and the
  referrers of a manifest with the `artifactType` filter.
- **Pushing.** Blob uploads in one request, in chunks with `Content-Range`, or
  streamed in one `PATCH` as docker sends them, and mounted from another
  repository. Every blob is checked against its digest before it is stored,
  SHA-256 and SHA-512 alike. `maxUploadSize` applies to a blob as a whole,
  `maxChunkSize` does not. Manifests are the OCI image manifest and index and
  Docker's schema 2 manifest and manifest list; a manifest is refused while a
  blob or manifest it refers to is not in the repository.
- **Deleting.** A tag, a manifest with the tags pointing to it, and a blob.

Cleanup runs with the hourly sweep: an upload nothing has been added to for
24 hours is removed, and so is a blob that no manifest of any repository refers
to and that has not been pushed or mounted in the last 24 hours. A manifest
stays until it is deleted, tagged or not: an image a newer push has taken the
tag from can still be pulled by its digest, and its blobs are kept for it.
An account that sets `registry` can also run it at once with **Clean up** on
the registry page (`POST ?go-fs=registry-cleanup`), which answers with how many
blobs it removed and how many bytes that freed. It removes the unreferenced
blobs older than 10 minutes rather than 24 hours, so what a deleted tag left
behind goes right away while a push under way keeps its layers; uploads are
swept as the hourly cleanup sweeps them.

**Signatures and attestations.** The registry stores the signatures,
attestations and SBOMs that tools push next to an image, and gives them back,
so `cosign verify`, `notation verify`, `gh attestation verify` and admission
controllers such as Kyverno or the Sigstore policy-controller can check images
pulled from it. They are ordinary manifests, attached to their image in one of
two ways:

| Tool | Attached by | What the registry page calls it |
|---|---|---|
| cosign 2 `sign` | the tag `sha256-<hex>.sig` | cosign signature |
| cosign 2 `attest` | the tag `sha256-<hex>.att` | SLSA provenance, SPDX SBOM, … by its predicate |
| cosign 2 `attach sbom` | the tag `sha256-<hex>.sbom` | SPDX SBOM or CycloneDX SBOM, not signed |
| cosign 3 `sign` | its subject, as a Sigstore bundle | Sigstore signature |
| GitHub `actions/attest-build-provenance`, `gh attestation` | its subject, as a Sigstore bundle | SLSA provenance |
| notation `sign` | its subject | Notation signature |
| oras `attach` and any other OCI 1.1 artifact | its subject | by its artifact type |

The referrers API, `GET /v2/<name>/referrers/<digest>`, lists what names a
manifest as its subject, with the `artifactType` filter; a `+` in the filter
that was not escaped is read as one. The `sha256-<hex>.sig` tags are stored as
the tags they are.

The registry page lists a tag's artifacts with it: the fallback tags of an
image the repository holds are not rows of their own, and the row of the
image shows a **signed** badge when one of them is a signature, **attested**
when one is a signed statement, whose title names each kind. **Details** lists
each artifact with its manifest, artifact type, fallback tag and push time,
and the platform it is for when it is for one image of an index rather than
the index itself.

The cleanup removes what belongs to an image that is gone: a manifest whose
subject no repository holds, and a fallback tag named after a digest no
repository holds, with the manifest it pointed to. Both only once they are
older than the cleanup's cutoff, as a copy may bring the signature before its
image, and never what another tag still points to or an index names. A
subject held by another repository counts, since cosign stores signatures
elsewhere with `COSIGN_REPOSITORY`. Removing one artifact can leave another
dangling, a signature of an SBOM say, which goes in the same run. An
untagged image that is still there keeps its artifacts, as it keeps its blobs.

**Verifying signatures.** Each `[[http.services.registryTrust]]` table names a
signer the registry checks signatures against, for every repository or for
those its `repositories` patterns name (`*` stands for anything, slashes
included):

```toml
[[http.services.registryTrust]]
name = "release"
publicKey = "/var/lib/go-fs/cosign.pub"           # cosign sign --key, cosign attest --key

[[http.services.registryTrust]]
name = "notary"
certificates = "/var/lib/go-fs/notation-ca.pem"   # notation sign

[[http.services.registryTrust]]
name = "github"
trustedRoot = "/var/lib/go-fs/trusted_root.json"  # keyless: cosign 3, GitHub attestations
identity = "https://github.com/acme/app/.github/workflows/release.yml@refs/tags/*"
issuer = "https://token.actions.githubusercontent.com"
repositories = ["acme/*"]
```

What is checked:

- **cosign signatures and attestations made with a key**, under their `.sig`
  and `.att` tags or as Sigstore bundles: ECDSA, Ed25519 and RSA public keys.
- **Keyless Sigstore bundles**, as cosign 3 and GitHub's attestations store
  them. The certificate has to chain to a Fulcio authority of the trusted
  root, name the identity and the issuer, and have been valid when the Rekor
  entry or the RFC 3161 timestamp says it was used. `gh attestation
  trusted-root > trusted_root.json` writes the roots of Sigstore's public
  instance and of GitHub's own, which signs the attestations of private
  repositories, one per line; both are read.
- **Notary Project signatures** in JWS envelopes, against the root
  certificates, with their expiry. COSE envelopes are listed but not checked.

The keyless check needs no network: the trusted root is read from its file,
never refreshed online, so update it when Sigstore rotates its keys. Legacy
keyless `.sig` signatures of cosign 2 are not checked.

A signature is checked when it is pushed, which the log records as `registry
signature verified`, with the trust and the signer, or `registry signature not
verified`, with the reason. The page then shows **verified** when a verified
signature is for what the tag points to or for each of its platforms,
**partly verified** when only some platforms have one, and **not verified**
when none verifies. **Details** names the trust and the signer of each, or why
it was not verified. A repository no trust is for shows **signed** as before.
The results are kept until the configuration is reloaded, and a change of the
trusts takes effect with the reload. A key or root the registry cannot read
is refused with the rest of the file.

Nothing is refused for these signatures: an unsigned or unverified image is
pushed and pulled as any other. Whoever relies on a signature checks it
where the image is pulled, with `cosign verify`, `notation verify`, `gh
attestation verify` or a policy of the cluster. What the registry checks is
for its admin to see. The signatures in an image's labels are the exception,
below.

**Signatures in the labels of an image.** Some images carry their signature
inside themselves rather than next to them: `image-sign.sh` of
container-image-validation signs the layers of an image with an Ed25519 key
from a PKCS #12 file and writes the signature into the image's configuration
with `umoci config`, which adds no layer:

| label | holds |
|---|---|
| `<prefix>.signature` | base64 of the Ed25519 signature over `LAYER_HASH` |
| `<prefix>.certchain` | base64 of the PEM of the signing certificate and its intermediates |
| `<prefix>.alg` | `ed25519` |

`LAYER_HASH` is the SHA-256 of the image's `rootfs.diff_ids`, one per line
with a newline after the last, as 64 hex digits, and that string is what is
signed. Each `[[http.services.imagesign]]` table names a signer of such
images:

```toml
[[http.services.imagesign]]
name = "mitel"
labelPrefix = ["org.mitel.imagesign"]
certificates = """
-----BEGIN CERTIFICATE-----
MIIBizCCAT2gAwIBAgIEJvSEJzAFBgMrZXAw...
-----END CERTIFICATE-----
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEA...
-----END PUBLIC KEY-----
"""
mode = "enforced"
repositories = ["prod/*"]
```

- **`labelPrefix`** lists the prefixes the labels may be under, one or
  several.
- **`certificates`** is the PEM itself, pasted in, as many blocks as there
  are. A `CERTIFICATE` has to be a CA's, root or intermediate, and the
  signing certificate has to chain to one of them and be valid now. A
  `PUBLIC KEY` is an Ed25519 key that is trusted as it is: a signing
  certificate that carries it is taken whoever issued it and whenever it
  expires. Base64 of the PEM works too, and the admin page has a box to paste
  it into, or a file to upload, and says what it holds.
- **`repositories`** limits the signer to the repositories it names, as for
  `registryTrust`.
- **`mode`** says what the registry refuses:

| mode | signature verifies | no signature | signature does not verify |
|---|---|---|---|
| `optional` (or empty) | **signed** | **⚠ unsigned**, taken and handed out | **⚠ bad signature**, taken and handed out |
| `mandatory` | **signed** | **⚠ unsigned**, taken and handed out | refused |
| `enforced` | **signed** | refused | refused |

A refused image is answered `403 DENIED` with the signer, the mode and the
reason, which `docker push` and `docker pull` print, and logged as `registry
push refused for the image's signature` or `registry pull refused …`. A pull,
an import or a push to another registry from the page is refused alike, and
stores or sends nothing. A push that is taken logs `registry image signature
verified` with the signer, or warns `registry image is not signed` or
`registry image signature does not verify`. Images stored before an entry
was made stricter are refused from the reload on, and their badge says so.

Each platform of an image is checked on its own, which matters because
`image-sign.sh` signs one platform: an index is always handed out, and a
client is refused the platform that is not signed. The page shows **partly
signed** for such an index, and a pull or a push from the page is told to
choose the platforms that are signed. Signatures, attestations, SBOMs and
other artifacts are no images and are never refused; neither is a blob,
which a client only finds by the manifest that is refused. Where several
entries are for a repository, a signature one of them verifies is enough and
the strictest mode applies.

What the signature does not cover: it is made over the layers alone, so the
entrypoint, the environment, the user an image runs as and its name are not
signed, and the same layers relabelled with another entrypoint still verify.
The registry takes the `diff_ids` from the configuration as they are, and
the container runtime checks them against the layers when it unpacks them.

- **Plain HTTP.** Docker only talks to a registry over plain HTTP when it is
  `localhost` or listed in `insecure-registries` of the daemon's
  configuration; use the TLS port everywhere else.
- **`http.connection.writeTimeout`** caps how long a pull of a large layer may take, as
  it caps any download.
