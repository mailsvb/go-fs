//go:build light || no_mp3tags

package mp3edit

import "log/slog"

// Compiled says the binary was built without the part.
const Compiled = false

func Genres() []string { return nil }

func Persist(Bucket, *slog.Logger) {}

func Read(Source) (Info, error) { return Info{}, ErrUnavailable }

func CoverOf(Source) ([]byte, string, error) { return nil, "", ErrUnavailable }

func Analyze(Source) (Analysis, error) { return Analysis{}, ErrUnavailable }

func PlanAlbum([]Source) (Album, error) { return Album{}, ErrUnavailable }

func Save(Source, Edit) error { return ErrUnavailable }
