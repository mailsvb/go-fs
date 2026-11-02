package httpd

import (
	"crypto/rand"
	"log/slog"

	"github.com/mailsvb/go-fs/internal/store"
)

// keptKey is the random key of size bytes kept at name in keys, made and kept
// there the first time. It reports whether the key is kept: one that cannot
// be is made for this run only, and the log says why, rather than keeping the
// server down. A kept key of another size, which no version wrote, is
// replaced.
func keptKey(keys *store.Bucket, name string, size int, logger *slog.Logger) ([]byte, bool, error) {
	random := func() ([]byte, error) {
		key := make([]byte, size)
		_, err := rand.Read(key)
		return key, err
	}
	if keys == nil {
		key, err := random()
		return key, false, err
	}
	var key []byte
	_, err := keys.Remembered(name, &key, func() (err error) {
		key, err = random()
		return err
	})
	if err == nil && len(key) == size {
		return key, true, nil
	}
	if err == nil {
		key, err = random()
		if err != nil {
			return nil, false, err
		}
		if err := keys.Put(name, key); err == nil {
			return key, true, nil
		}
	}
	logger.Warn("cannot keep a generated key in the persistent data file, so it lasts until the next restart",
		"key", name, "error", err)
	key, err = random()
	return key, false, err
}
