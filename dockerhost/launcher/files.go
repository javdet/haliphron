package launcher

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// The agent's uid and gid, as the image's USER line sets them.
const (
	AgentUID = 1000
	AgentGID = 1000
)

// fileKey is what a Kubernetes Secret or ConfigMap admits as a key. The same
// rule here keeps a key one runtime accepts from becoming a path the other
// writes outside its directory.
var fileKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

// WriteFiles writes an attempt's directory from scratch: secrets/ with files
// 0400 in a 0500 directory, role/ with files 0444 in a 0555 directory, both
// owned by the agent user when chown is true. A previous copy is removed first,
// so a key dropped from the materials does not linger.
//
// chown is false only in tests that do not run as root. In production the
// controller runs as root in its container, with CAP_CHOWN, precisely so these
// files can belong to uid 1000 and nobody else.
func WriteFiles(dir string, f Files, chown bool) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("launcher: clear %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return fmt.Errorf("launcher: create %s: %w", dir, err)
	}
	if err := writeSet(filepath.Join(dir, "secrets"), f.Secrets, 0o400, 0o500, chown); err != nil {
		return err
	}
	if len(f.Role) > 0 {
		if err := writeSet(filepath.Join(dir, "role"), f.Role, 0o444, 0o555, chown); err != nil {
			return err
		}
	}
	return nil
}

func writeSet(dir string, files map[string][]byte, fileMode, dirMode os.FileMode, chown bool) error {
	// Created writable, filled, then narrowed: a 0500 directory cannot be
	// written into even by its owner.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("launcher: create %s: %w", dir, err)
	}
	for _, name := range SortedKeys(files) {
		if !fileKey.MatchString(name) || name == "." || name == ".." {
			return fmt.Errorf("launcher: %q is not a valid material key", name)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, files[name], 0o600); err != nil {
			return fmt.Errorf("launcher: write %s: %w", path, err)
		}
		if chown {
			if err := os.Chown(path, AgentUID, AgentGID); err != nil {
				return fmt.Errorf("launcher: chown %s: %w", path, err)
			}
		}
		if err := os.Chmod(path, fileMode); err != nil {
			return fmt.Errorf("launcher: chmod %s: %w", path, err)
		}
	}
	if chown {
		if err := os.Chown(dir, AgentUID, AgentGID); err != nil {
			return fmt.Errorf("launcher: chown %s: %w", dir, err)
		}
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("launcher: chmod %s: %w", dir, err)
	}
	return nil
}

// RemoveFiles removes an attempt's directory. A 0500 directory is made
// writable first, or RemoveAll cannot unlink what is inside it.
func RemoveFiles(dir string) error {
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("launcher: remove %s: %w", dir, err)
	}
	return nil
}
