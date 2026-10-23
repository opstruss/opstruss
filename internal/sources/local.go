package sources

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LocalHash preserves the original path/content hash of local working trees.
func LocalHash(dir string) (string, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case !d.Type().IsRegular():
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s  %s\n", payloadHash(b), filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("plugin %s: %w", dir, err)
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "")))
	return "h1:" + base64.StdEncoding.EncodeToString(sum[:]), nil
}
