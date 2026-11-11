package logging

import (
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Masked is what a secret is written as in a record.
const Masked = "***"

// Secret is a value that is never written to the log: a record shows it as
// Masked, whichever handler and format write it.
type Secret string

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value {
	return slog.StringValue(Masked)
}

// sensitiveNames are the query parameters, form fields and arguments whose
// value is a credential, compared without case. Short names are the
// OpenSubsonic ones: p is a password, t a salted hash of it and s the salt.
var sensitiveNames = map[string]bool{
	"p": true, "t": true, "s": true,
	"apikey": true, "api_key": true, "key": true,
	"token": true, "access_token": true, "refresh_token": true,
	"password": true, "pw": true, "php_dls_pw": true,
	"secret": true, "client_secret": true,
	"sig": true, "signature": true,
	"x-amz-signature": true, "x-amz-credential": true, "x-amz-security-token": true,
	// the key of a Sonos speaker's request for a file it plays
	"go-fs-play": true,
	// the upload session of a registry, which grants writing to it
	"_state": true,
}

// sensitiveParts make any name holding them a credential's, whatever else it
// says: an X-Api-Key header, a vaultPassword argument.
var sensitiveParts = []string{"token", "secret", "password", "passwd", "signature", "credential", "apikey", "api-key", "api_key"}

// SensitiveName reports whether a parameter, field, header or argument of
// that name holds a credential, so that its value has to be masked.
func SensitiveName(name string) bool {
	lower := strings.ToLower(strings.TrimLeft(name, "-"))
	if sensitiveNames[lower] {
		return true
	}
	for _, part := range sensitiveParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

// URL is a URL as a record shows it: the scheme, the host, the path and the
// names of the query, with the value of every parameter that holds a
// credential masked, and without a password or a fragment. A request's URL,
// which has no host, comes out as its path and query.
func URL(u *url.URL) string {
	if u == nil {
		return ""
	}
	shown := *u
	if shown.User != nil {
		if _, has := shown.User.Password(); has {
			shown.User = url.UserPassword(shown.User.Username(), Masked)
		}
	}
	shown.RawQuery = Query(shown.RawQuery)
	shown.Fragment, shown.RawFragment = "", ""
	return shown.String()
}

// URLString is URL for a URL that is still text, such as a Location header.
// Text that does not parse is masked whole, since what it holds is unknown.
func URLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Masked
	}
	return URL(u)
}

// Query masks the values of the credentials in a raw query, leaving the rest
// of it as it was sent: the order, the encoding and the parameters without a
// value.
func Query(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	for i, part := range parts {
		name, _, hasValue := strings.Cut(part, "=")
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			decoded = name
		}
		if hasValue && SensitiveName(decoded) {
			parts[i] = name + "=" + Masked
		}
	}
	return strings.Join(parts, "&")
}

// maxHeaderValue is how much of one header value a record keeps; a value
// longer than that is cut, so a client sending a huge header cannot make a
// huge record.
const maxHeaderValue = 512

// Headers is a header as a record shows it: a group of every field, with
// what authenticates masked. Authorization keeps its scheme, Cookie and
// Set-Cookie the names of the cookies, and a URL in a header has its query
// masked as URL masks it. Fields are in the order of their names, so two
// records of the same exchange compare line by line.
func Headers(h http.Header) slog.Value {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	attrs := make([]slog.Attr, 0, len(names))
	for _, name := range names {
		values := make([]string, len(h[name]))
		for i, value := range h[name] {
			values[i] = headerValue(name, value)
		}
		attrs = append(attrs, slog.String(name, cut(strings.Join(values, ", "))))
	}
	return slog.GroupValue(attrs...)
}

// tokenParam is a Token="…" of the MediaBrowser scheme Jellyfin clients log
// in with, the one part of it that is a credential.
var tokenParam = regexp.MustCompile(`(?i)(token\s*=\s*)("[^"]*"|[^,\s]*)`)

func headerValue(name, value string) string {
	switch http.CanonicalHeaderKey(name) {
	case "Authorization", "Proxy-Authorization", "X-Emby-Authorization":
		scheme, rest, _ := strings.Cut(strings.TrimSpace(value), " ")
		switch strings.ToLower(scheme) {
		case "mediabrowser", "emby":
			// the client and the device it names are what a trace of a
			// Jellyfin client needs; only the token is secret
			return scheme + " " + tokenParam.ReplaceAllString(rest, "${1}"+Masked)
		}
		if strings.Contains(scheme, "=") {
			// X-Emby-Authorization sent without its scheme
			return tokenParam.ReplaceAllString(value, "${1}"+Masked)
		}
		return scheme + " " + Masked
	case "Cookie":
		cookies := strings.Split(value, ";")
		for i, cookie := range cookies {
			name, _, _ := strings.Cut(strings.TrimSpace(cookie), "=")
			cookies[i] = name + "=" + Masked
		}
		return strings.Join(cookies, "; ")
	case "Set-Cookie":
		cookie, attributes, hasAttributes := strings.Cut(value, ";")
		name, _, _ := strings.Cut(strings.TrimSpace(cookie), "=")
		if hasAttributes {
			return name + "=" + Masked + ";" + attributes
		}
		return name + "=" + Masked
	case "Location", "Content-Location", "Referer", "Origin":
		return URLString(value)
	}
	if SensitiveName(name) {
		return Masked
	}
	return value
}

func cut(value string) string {
	if len(value) <= maxHeaderValue {
		return value
	}
	return value[:maxHeaderValue] + "…"
}

// Args are the arguments of a program as a record shows them: a URL among
// them is shown as URL shows it, and the value of an option that names a
// credential is masked, both as "--password value" and as "--password=value".
func Args(args []string) []string {
	shown := make([]string, len(args))
	maskNext := false
	for i, arg := range args {
		switch {
		case maskNext:
			shown[i] = Masked
			maskNext = false
			continue
		case strings.HasPrefix(arg, "-"):
			name, _, hasValue := strings.Cut(arg, "=")
			// a one letter option is not the OpenSubsonic parameter of
			// the same name: ffmpeg's -s is a size and -t a duration
			if len(strings.TrimLeft(name, "-")) > 1 && SensitiveName(name) {
				if hasValue {
					shown[i] = name + "=" + Masked
				} else {
					shown[i] = arg
					maskNext = true
				}
				continue
			}
		case strings.Contains(arg, "://"):
			if u, err := url.Parse(arg); err == nil && u.Scheme != "" {
				shown[i] = URL(u)
				continue
			}
		}
		shown[i] = arg
	}
	return shown
}
