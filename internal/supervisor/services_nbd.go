//go:build !light && !no_nbd

package supervisor

import (
	"log/slog"

	"github.com/mailsvb/go-fs/internal/nbd"
	"github.com/mailsvb/go-fs/internal/service"
)

// The NBD server, left out by the tag no_nbd and by light.
func init() {
	register(entry{
		name:    "nbd",
		order:   orderNBD,
		enabled: func(e env) bool { return e.cfg.NBD.Enabled },
		create: func(e env, log *slog.Logger) (service.Server, error) {
			return nbd.New(e.cfg.NBD, e.cfg.NBDUsers(), log)
		},
		reload: func(s service.Server, e env) error {
			return s.(*nbd.Server).Reload(e.cfg.NBD, e.cfg.NBDUsers())
		},
	})
}
