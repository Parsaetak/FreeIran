//go:build windows

package tunnel

import (
	"os"
)

// osGetEnvReal returns os.Getenv.
func osGetEnvReal(key string) string { return os.Getenv(key) }

// osStatReal returns os.Stat.
func osStatReal(path string) (interface{}, error) { return os.Stat(path) }

// osRemoveReal returns os.Remove.
func osRemoveReal(path string) error { return os.Remove(path) }

// osExecutableReal returns os.Executable.
func osExecutableReal() (string, error) { return os.Executable() }
