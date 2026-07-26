package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/skflowne/portolan/internal/core"
)

// SocketPath derives the project-keyed control-socket path for cfg. Explicit
// paths are used verbatim; default paths live in a private per-user runtime
// directory rather than the shared temporary directory.
func SocketPath(cfg core.Config) string {
	if cfg.ControlSocket != "" {
		return cfg.ControlSocket
	}
	runtimeDir := controlRuntimeDir()
	if runtimeDir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(cfg.ProjectRoot))
	return filepath.Join(runtimeDir, fmt.Sprintf("portoland-%s.sock", hex.EncodeToString(sum[:])[:12]))
}

func controlRuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" && filepath.IsAbs(base) {
		return filepath.Join(base, "portoland")
	}
	if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		// A user-owned cache parent is a safer fallback than a predictable name
		// directly beneath the shared temporary directory.
		return filepath.Join(cache, "portoland-runtime")
	}
	return ""
}

func ensureControlRuntimeDir() error {
	dir := controlRuntimeDir()
	if dir == "" {
		return errors.New("no private runtime base: set XDG_RUNTIME_DIR or HOME")
	}
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" && filepath.IsAbs(base) {
		if err := validatePrivateDir(base); err != nil {
			return fmt.Errorf("unsafe XDG_RUNTIME_DIR %s: %w", base, err)
		}
	} else if cache, err := os.UserCacheDir(); err == nil && filepath.IsAbs(cache) {
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return fmt.Errorf("creating user cache directory: %w", err)
		}
		if err := validateUserOwnedDir(cache); err != nil {
			return fmt.Errorf("unsafe user cache directory %s: %w", cache, err)
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	return validatePrivateDir(dir)
}

func validateUserOwnedDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("permissions %04o allow group or other writes", info.Mode().Perm())
	}
	if !ownedByEffectiveUser(info) {
		return errors.New("not owned by the effective user")
	}
	return nil
}

func validatePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("permissions %04o allow group or other access", info.Mode().Perm())
	}
	if !ownedByEffectiveUser(info) {
		return errors.New("not owned by the effective user")
	}
	return nil
}

func ownershipLockPath(socketPath string) string {
	absolute, err := filepath.Abs(socketPath)
	if err != nil {
		absolute = socketPath
	}
	sum := sha256.Sum256([]byte(absolute))
	return filepath.Join(controlRuntimeDir(), fmt.Sprintf("lock-%s", hex.EncodeToString(sum[:])[:24]))
}
