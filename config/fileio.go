package config

import (
	"fmt"
	"os"
)

// decodeFile reads the file at path and decodes it onto out. Errors name the file.
func decodeFile(path string, out any) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	keys, err := decodeStrict(data, out)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return keys, nil
}
