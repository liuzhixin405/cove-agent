//go:build !windows

package browser

import "os"

func privateWorkflowDirectory(directory string) error { return os.Chmod(directory, 0700) }
