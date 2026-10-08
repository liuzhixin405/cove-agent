//go:build !windows

package remote

import "os"

func privateCredential(file *os.File) error { return file.Chmod(0600) }
