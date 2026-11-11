package remote

import "log/slog"

// LogValue implements slog.LogValuer: a login is where it goes and as whom,
// with the password, the token and the password of the vault only said to be
// set or not, so that logging one can never write them out.
func (l Login) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("type", l.Type),
		slog.String("server", l.Server),
		slog.String("host", l.Host),
		slog.Int("port", l.Port),
		slog.String("username", l.Username),
		slog.Bool("password", l.Password != ""),
		slog.String("hostKey", l.HostKey),
		slog.String("url", l.URL),
		slog.Bool("token", l.Token != ""),
		slog.String("vaultPath", l.VaultPath),
		slog.Bool("vaultPassword", l.VaultPassword != ""))
}
