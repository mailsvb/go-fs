package imagesig

import "encoding/base64"

func encB64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
