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

[http]
enabled = true
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
| Pulling | anyone while `http.registryAnonymousRead = true` (the default), otherwise an account that sets `registry` |
| Pushing, tagging, deleting | an account that sets `registry` |
| Credentials | a token from `/v2/_token`, fetched with the account's username and password as `docker login` does, or HTTP Basic on every request |

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
sessions, `http.httpSessionTokenSecret`, but cannot stand in for one, and are
accepted for five minutes; changing the account's password ends them at once.
A request may also carry Basic credentials itself, which is what `curl -u`
sends. A wrong password counts against the address in the same lock as
everywhere else, so `docker login` with a wrong one fails as it should.

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
- **Push to registry…** copies a tag to the image named, prefilled with the
  tag's own name: put the registry in front, `ghcr.io/my-org/team/app:1.0`,
  or change the name and tag as well. Validate proves that the login may push
  there, says whether the tag is there already, and lists the architectures
  of the tag here. Blobs the other registry already has are not sent again.

Both take an optional username and password (or access token) for the other
registry; they are used for that one transfer, never stored and never logged.
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
`http.registryAnonymousRead = false` a visitor is sent to the login page
first. The form then also accepts accounts that set only `registry`; their
session reaches the registry page and nothing in the file tree.

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

- **Plain HTTP.** Docker only talks to a registry over plain HTTP when it is
  `localhost` or listed in `insecure-registries` of the daemon's
  configuration; use the TLS port everywhere else.
- **`http.writeTimeout`** caps how long a pull of a large layer may take, as
  it caps any download.
