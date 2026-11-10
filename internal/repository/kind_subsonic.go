//go:build !light && !no_subsonic

package repository

import "github.com/mailsvb/go-fs/internal/config"

// The OpenSubsonic music library is left out by the tag no_subsonic, and by
// light. Its kind is the music library of kind_music.go.
func init() { kinds[config.RepositoryTypeSubsonic] = newMusicKind }
