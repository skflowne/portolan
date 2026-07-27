// Package pathnorm owns canonical host-path identity and file-URI conversion.
// Canonical paths are lexical, absolute Linux/WSL paths; conversion never
// resolves symlinks or requires a path to exist.
package pathnorm

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
)

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isDriveLetterPath(p string) bool {
	if len(p) < 2 || !isASCIILetter(p[0]) || p[1] != ':' {
		return false
	}
	return len(p) == 2 || p[2] == '\\' || p[2] == '/'
}

func isUNCPath(p string) bool {
	return strings.HasPrefix(p, `\\`)
}

// IsWindowsPath reports whether p uses a drive letter or UNC syntax.
func IsWindowsPath(p string) bool {
	return isDriveLetterPath(p) || isUNCPath(p)
}

func currentDistro(input, distro string) error {
	current := os.Getenv("WSL_DISTRO_NAME")
	if current == "" {
		return fmt.Errorf("pathnorm: cannot represent %q because WSL_DISTRO_NAME is unset", input)
	}
	if !strings.EqualFold(distro, current) {
		return fmt.Errorf("pathnorm: distro %q in %q does not match current distro %q", distro, input, current)
	}
	return nil
}

func canonicalWSLPath(input, server, distro string, rest []string) (string, error) {
	if !strings.EqualFold(server, "wsl$") && !strings.EqualFold(server, "wsl.localhost") {
		return "", fmt.Errorf("pathnorm: unsupported network path %q", input)
	}
	if distro == "" {
		return "", fmt.Errorf("pathnorm: malformed WSL path %q: missing distro", input)
	}
	if err := currentDistro(input, distro); err != nil {
		return "", err
	}
	return path.Clean("/" + strings.Join(rest, "/")), nil
}

func canonicalUNC(p string) (string, error) {
	tail := p[2:]
	if tail == "" || tail[0] == '\\' || tail[0] == '/' {
		return "", fmt.Errorf("pathnorm: malformed UNC path %q", p)
	}
	segments := strings.Split(strings.ReplaceAll(tail, `\`, "/"), "/")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return "", fmt.Errorf("pathnorm: malformed UNC path %q", p)
	}
	return canonicalWSLPath(p, segments[0], segments[1], segments[2:])
}

func splitMountDrive(p string) (drive, rest string, ok bool) {
	const prefix = "/mnt/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	rem := p[len(prefix):]
	if rem == "" || !isASCIILetter(rem[0]) || len(rem) > 1 && rem[1] != '/' {
		return "", "", false
	}
	drive = strings.ToLower(string(rem[0]))
	if len(rem) > 1 {
		rest = rem[1:]
	}
	return drive, rest, true
}

// Canonicalize returns the strict canonical host identity for p.
func Canonicalize(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("pathnorm: empty path")
	}
	if strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("pathnorm: path %q contains NUL", p)
	}
	if isUNCPath(p) {
		return canonicalUNC(p)
	}
	if strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("pathnorm: unsupported network path %q", p)
	}
	if strings.HasPrefix(p, "/") {
		cleaned := path.Clean(p)
		if drive, rest, ok := splitMountDrive(cleaned); ok {
			return "/mnt/" + drive + rest, nil
		}
		return cleaned, nil
	}
	if isDriveLetterPath(p) {
		if len(p) == 2 {
			return "", fmt.Errorf("pathnorm: %q is a bare drive", p)
		}
		drive := strings.ToLower(string(p[0]))
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		cleaned := path.Clean("/" + strings.TrimLeft(rest, "/"))
		if cleaned == "/" {
			return "/mnt/" + drive, nil
		}
		return "/mnt/" + drive + cleaned, nil
	}
	return "", fmt.Errorf("pathnorm: %q is not an absolute host path", p)
}

// Normalize is retained until callers migrate to Canonicalize.
// Deprecated: use Canonicalize and handle its error.
func Normalize(p string) string {
	if canonical, err := Canonicalize(p); err == nil {
		return canonical
	}
	if p == "" {
		return ""
	}
	return path.Clean(strings.ReplaceAll(p, `\`, "/"))
}

// WSLToWindows converts a canonicalizable host path to Windows syntax.
func WSLToWindows(p string) (string, error) {
	hostPath, err := Canonicalize(p)
	if err != nil {
		return "", err
	}
	if drive, rest, ok := splitMountDrive(hostPath); ok {
		return strings.ToUpper(drive) + `:\` + strings.ReplaceAll(strings.TrimPrefix(rest, "/"), "/", `\`), nil
	}
	distro := os.Getenv("WSL_DISTRO_NAME")
	if distro == "" {
		return "", fmt.Errorf("pathnorm: cannot represent %q in Windows syntax because WSL_DISTRO_NAME is unset", hostPath)
	}
	return `\\wsl.localhost\` + distro + strings.ReplaceAll(hostPath, "/", `\`), nil
}

// PathToURI converts a canonicalizable host path to a file URI.
func PathToURI(p string) (string, error) {
	hostPath, err := Canonicalize(p)
	if err != nil {
		return "", err
	}
	u := &url.URL{Scheme: "file", Path: hostPath}
	if hasDriveURIPath(hostPath) {
		escaped := u.EscapedPath()
		u.RawPath = escaped[:2] + "%3A" + escaped[3:]
	}
	return u.String(), nil
}

func hasDriveURIPath(p string) bool {
	return len(p) >= 3 && p[0] == '/' && isASCIILetter(p[1]) && p[2] == ':'
}

func hasEscapedDriveColon(rawPath string) bool {
	return len(rawPath) >= 5 && rawPath[0] == '/' && isASCIILetter(rawPath[1]) && strings.EqualFold(rawPath[2:5], "%3a")
}

func uriWSLPath(uri string, u *url.URL) (string, error) {
	tail := strings.TrimPrefix(u.Path, "/")
	if tail == "" {
		return "", fmt.Errorf("pathnorm: %q has no WSL distro path", uri)
	}
	segments := strings.Split(tail, "/")
	return canonicalWSLPath(uri, u.Host, segments[0], segments[1:])
}

// URIToPath converts a supported file URI to canonical host identity.
func URIToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("pathnorm: invalid URI %q: %w", uri, err)
	}
	if !strings.EqualFold(u.Scheme, "file") {
		return "", fmt.Errorf("pathnorm: %q is not a file URI", uri)
	}
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("pathnorm: unsupported file URI %q", uri)
	}
	if strings.EqualFold(u.Host, "wsl$") || strings.EqualFold(u.Host, "wsl.localhost") {
		return uriWSLPath(uri, u)
	}
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", fmt.Errorf("pathnorm: unsupported file URI authority %q", u.Host)
	}
	if u.Path == "" {
		return "", fmt.Errorf("pathnorm: %q has an empty path", uri)
	}
	p := u.Path
	if hasDriveURIPath(p) && !hasEscapedDriveColon(u.RawPath) {
		p = p[1:]
	}
	return Canonicalize(p)
}
