package repository

import "github.com/mailsvb/go-fs/internal/config"

// The OpenSubsonic music library. Its kind is the music library of
// kind_music.go.
func init() { kinds[config.RepositoryTypeSubsonic] = newMusicKind }
