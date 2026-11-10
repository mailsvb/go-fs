//go:build !light && !no_jellyfin

package repository

import "github.com/mailsvb/go-fs/internal/config"

// The Jellyfin music library is left out by the tag no_jellyfin, and by
// light. Its kind is the music library of kind_music.go.
func init() { kinds[config.RepositoryTypeJellyfin] = newMusicKind }
