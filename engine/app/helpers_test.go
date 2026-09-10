package app

import "os"

// writeFile is a test helper for creating legacy fixture files.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
