package wardogs

import (
	"strings"
)

// Redacted replaces a secret's value in a redacted configuration document.
const Redacted = "<redacted>"

// IsSecretKey reports whether a configuration key holds a secret: the RCON password and its
// hash, the feed's token and its URL (it names the sink), and the join password. A key with one
// of the generic secret names is a secret in any section, so a section renamed by a new build
// does not leak it.
func IsSecretKey(section, key string) bool {
	switch {
	case strings.EqualFold(key, "Password"), strings.EqualFold(key, "PasswordHash"),
		strings.EqualFold(key, "Token"), strings.EqualFold(key, "ServerPassword"):
		return true
	case strings.EqualFold(key, "Url"):
		return strings.HasSuffix(strings.ToLower(section), "wdserverfeed")
	}
	return false
}

// RedactConfig replaces every non-empty secret value in a configuration document with
// [Redacted]. Everything else is kept byte for byte, line endings included (the document is
// CRLF on the wire).
func RedactConfig(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	section := ""
	for line := range strings.SplitAfterSeq(text, "\n") {
		body, eol := splitEOL(line)
		trimmed := strings.TrimSpace(body)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		} else if key, value, ok := strings.Cut(body, "="); ok {
			name := strings.TrimLeft(strings.TrimSpace(key), "+-.!")
			if IsSecretKey(section, name) && strings.TrimSpace(value) != "" {
				body = key + "=" + Redacted
			}
		}
		b.WriteString(body)
		b.WriteString(eol)
	}
	return b.String()
}

// splitEOL splits a line into its content and its ending ("\r\n", "\n" or "").
func splitEOL(line string) (string, string) {
	if strings.HasSuffix(line, "\r\n") {
		return line[:len(line)-2], "\r\n"
	}
	if strings.HasSuffix(line, "\n") {
		return line[:len(line)-1], "\n"
	}
	return line, ""
}

// NormalizeEOL turns CRLF and lone CR into LF, so two documents compare by content.
func NormalizeEOL(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// ToCRLF writes a document with CRLF line endings, as the server sends and expects it.
func ToCRLF(s string) string {
	return strings.ReplaceAll(NormalizeEOL(s), "\n", "\r\n")
}
