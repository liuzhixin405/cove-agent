//go:build !windows

package fsatomic

import "os"

func renameRootFile(root *os.Root, from, to string) error {
	return root.Rename(from, to)
}
