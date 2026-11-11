package config

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/mailsvb/go-fs/internal/logging"
)

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

// LogValue implements slog.LogValuer: the whole configuration as the file
// spells its keys, "http.port" and "users[0].username", one attribute each,
// with the value of every key that holds a credential masked, so that a
// record of the configuration in effect can be handed over with the log.
func (c Config) LogValue() slog.Value {
	var attrs []slog.Attr
	flatten(reflect.ValueOf(c), "", &attrs)
	return slog.GroupValue(attrs...)
}

// credentialKey reports whether a key of the file holds a credential or a
// key: a password, a token or its hash, a secret, a private key.
func credentialKey(name string) bool {
	lower := strings.ToLower(name)
	for _, part := range []string{"password", "secret", "token", "hash", "key", "credential", "signature"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

func flatten(v reflect.Value, path string, attrs *[]slog.Attr) {
	switch v.Kind() {
	case reflect.Struct:
		if v.Type().PkgPath() != configPackage {
			*attrs = append(*attrs, slog.Any(path, v.Interface()))
			return
		}
		for i := range v.NumField() {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("toml"), ",")
			switch name {
			case "-":
				continue
			case "":
				name = strings.ToLower(field.Name[:1]) + field.Name[1:]
			}
			key := name
			if path != "" {
				key = path + "." + name
			}
			// a credential is text; a lifetime or a list of tokens is
			// not one itself, whatever its name says
			if credentialKey(name) && isText(v.Field(i)) && !isZero(v.Field(i)) {
				*attrs = append(*attrs, slog.String(key, logging.Masked))
				continue
			}
			flatten(v.Field(i), key, attrs)
		}
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			return
		}
		// a list of plain values is one attribute, a list of entries one
		// per field of each
		if k := v.Type().Elem().Kind(); k != reflect.Struct && k != reflect.Pointer {
			*attrs = append(*attrs, slog.Any(path, v.Interface()))
			return
		}
		for i := range v.Len() {
			flatten(v.Index(i), fmt.Sprintf("%s[%d]", path, i), attrs)
		}
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		flatten(v.Elem(), path, attrs)
	case reflect.Map:
		if v.Len() > 0 {
			*attrs = append(*attrs, slog.Int(path+".entries", v.Len()))
		}
	default:
		if v.CanInterface() {
			*attrs = append(*attrs, slog.Any(path, v.Interface()))
		}
	}
}

// isText reports whether v is a string, or a list of strings or bytes.
func isText(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return true
	case reflect.Slice:
		k := v.Type().Elem().Kind()
		return k == reflect.String || k == reflect.Uint8
	}
	return false
}

func isZero(v reflect.Value) bool {
	return v.IsZero() || (v.Kind() == reflect.Slice && v.Len() == 0)
}
