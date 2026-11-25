package supervisor

import (
	"log/slog"

	"github.com/mailsvb/go-fs/internal/service"
	"github.com/mailsvb/go-fs/internal/sftp"
)

// The SFTP server.
func init() {
	register(entry{
		name:    "sftp",
		order:   orderSFTP,
		enabled: func(e env) bool { return e.cfg.SFTP.Enabled },
		create: func(e env, log *slog.Logger) (service.Server, error) {
			return sftp.New(e.cfg.SFTP, e.cfg.General.SSH, e.cfg.SFTPUsers(), e.store.Bucket(keys), log)
		},
		reload: func(s service.Server, e env) error {
			return s.(*sftp.Server).Reload(e.cfg.SFTP, e.cfg.General.SSH, e.cfg.SFTPUsers())
		},
	})
}
