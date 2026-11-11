package config

import "log/slog"

// The credentials of the configuration are never written to the log, even
// by a record that logs a whole entry: what a record shows of one is below,
// with a credential only said to be set or not.

// LogValue implements slog.LogValuer.
func (s Server) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", s.Name),
		slog.String("type", s.Type),
		slog.String("host", s.Host),
		slog.Int("port", s.Port),
		slog.String("username", s.Username),
		slog.Bool("password", s.Password != ""),
		slog.String("hostKeyFingerprint", s.HostKeyFingerprint),
		slog.String("url", s.URL),
		slog.Bool("token", s.Token != ""),
		slog.String("vaultPath", s.VaultPath),
		slog.Bool("vaultPassword", s.VaultPassword != ""),
		slog.Any("allowedUsers", s.AllowedUsers))
}

// LogValue implements slog.LogValuer.
func (u User) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("username", u.Username),
		slog.Bool("password", u.Password != ""),
		slog.Bool("ftp", u.FTP),
		slog.Bool("sftp", u.SFTP),
		slog.Bool("http", u.HTTP),
		slog.Bool("nbd", u.NBD),
		slog.Bool("s3", u.S3))
}

// LogValue implements slog.LogValuer. The hash is not the token, but it is
// what a token is checked against, so it is kept out as well.
func (t Token) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", t.Name),
		slog.String("expires", t.Expires),
		slog.Any("paths", t.Paths))
}
