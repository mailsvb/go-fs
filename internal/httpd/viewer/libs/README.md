# libs

The libraries of the document viewer, pdf.js, the viewer of Office Open XML
and the editor Ace, are fetched into this folder at build time and are not
committed:

```shell
make viewer-deps    # or: go run ./tools/viewer
```

`../libraries.lock` pins the versions and the sha512 of each tarball. A binary
built before the fetch has no document viewer; it says so when it starts.
