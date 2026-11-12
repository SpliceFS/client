package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

const yamlExt = ".yaml"

// yamlFiles returns the names of the files in dir that are read as configuration, in
// byte order. A missing dir is an empty one, so a fresh install needs no files at all.
// Directories, hidden names and names without the .yaml extension are skipped because
// editors and sync tools leave backups and temporary files next to the real ones.
func yamlFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, yamlExt) && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}
	return names, nil
}
