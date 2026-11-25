package selfupdate

import _ "embed"

// Product is the name of what this build is. It names the update files of a
// release and is what main shows next to the version.
const Product = "go-fs"

// keysFile holds the public keys go-fs trusts, one base64 key per line. It is
// compiled in, so nothing at runtime — not the configuration file, not the
// admin interface — can add a key.
//
//go:embed keys.txt
var keysFile string
