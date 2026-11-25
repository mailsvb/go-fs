package repository

import "github.com/mailsvb/go-fs/internal/config"

// The Jellyfin music library. Its kind is the music library of kind_music.go.
func init() { kinds[config.RepositoryTypeJellyfin] = newMusicKind }
