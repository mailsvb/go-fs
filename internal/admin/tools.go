package admin

import "github.com/mailsvb/go-fs/internal/config"

// toolPlaceholders say, in the empty box of each key of general.tools, what
// leaving it empty does on this machine: which program would be taken, or
// that none was found. Looking for one runs it only the first time, so the
// page asks every time it is opened, and shows a program installed since.
func toolPlaceholders() map[string]string {
	placeholders := make(map[string]string)
	for _, kind := range []config.ToolKind{config.ToolYtDlp, config.ToolFFmpeg} {
		text := "empty: not found on this system, so what needs " + kind.Name + " is off"
		if tool := config.FindTool(kind, ""); tool.Found() {
			text = "empty: found " + tool.Path + " (" + tool.Version + ")"
		}
		placeholders["general.tools."+kind.Key] = text
	}
	return placeholders
}
