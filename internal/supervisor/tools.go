package supervisor

import "github.com/mailsvb/go-fs/internal/config"

// logTools says which programs of general.tools were found, at the start
// and whenever that changes, so that the log tells why Fetch offers video or
// does not. The caller holds s.mu.
func (s *Supervisor) logTools(cfg config.Config) {
	for _, tool := range []struct {
		kind     config.ToolKind
		was, is  config.Tool
		neededBy string
	}{
		{config.ToolYtDlp, s.current.HTTP.YtDlp, cfg.HTTP.YtDlp, "fetching video and audio"},
		{config.ToolFFmpeg, s.current.HTTP.FFmpeg, cfg.HTTP.FFmpeg, "the best video and MP3 audio"},
	} {
		if s.applied && tool.was == tool.is {
			continue
		}
		if !tool.is.Found() {
			s.log.Info("tool not found, "+tool.neededBy+" is off", "tool", tool.kind.Name,
				"key", "general.tools."+tool.kind.Key)
			continue
		}
		source := "found"
		if tool.is.Configured {
			source = "configured"
		}
		s.log.Info("tool found", "tool", tool.kind.Name, "path", tool.is.Path,
			"version", tool.is.Version, "source", source)
	}
}
