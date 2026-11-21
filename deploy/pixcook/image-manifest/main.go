// Generate checksums using the application's migration whitespace convention.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func buildManifest(directory string) (map[string]string, error) {
	paths, err := filepath.Glob(filepath.Join(directory, "*.sql"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no SQL migrations in %s", directory)
	}
	result := make(map[string]string, len(paths))
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		checksum := sha256.Sum256([]byte(strings.TrimSpace(string(content))))
		result[filepath.Base(path)] = fmt.Sprintf("%x", checksum)
	}
	return result, nil
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: image-manifest MIGRATION_DIRECTORY OUTPUT_JSON")
	}
	manifest, err := buildManifest(args[0])
	if err != nil {
		return err
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(args[1], append(content, '\n'), 0644)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
