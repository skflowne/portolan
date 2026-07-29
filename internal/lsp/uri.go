package lsp

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// uriFromPath encodes an already host-normalized absolute path for the LSP
// wire. net/url preserves the required escaping of spaces and other special
// characters.
func uriFromPath(absPath string) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	return u.String()
}

func pathFromURI(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("lsp: parsing uri %q: %w", uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("lsp: unsupported uri scheme %q in %q", u.Scheme, uri)
	}
	return u.Path, nil
}

func languageIDForFile(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tsx":
		return "typescriptreact"
	case ".jsx":
		return "javascriptreact"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	default:
		return "typescript"
	}
}
