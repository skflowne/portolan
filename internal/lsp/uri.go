package lsp

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/skflowne/portolan/internal/pathnorm"
)

func pathFromURI(uri string) (string, error) {
	path, err := pathnorm.URIToPath(uri)
	if err != nil {
		return "", fmt.Errorf("lsp: decoding location URI %q: %w", uri, err)
	}
	return path, nil
}

// languageIDForFile picks the LSP languageId for didOpen based on extension.
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
