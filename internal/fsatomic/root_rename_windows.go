package fsatomic

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func renameRootFile(root *os.Root, from, to string) error {
	parent, err := root.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(windows.Handle(parent.Fd()), &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return err
	}
	if length >= uint32(len(buffer)) {
		return fmt.Errorf("rooted replacement directory path is too long")
	}
	dir := windows.UTF16ToString(buffer[:length])
	fromName, err := windows.UTF16PtrFromString(filepath.Join(dir, filepath.Base(from)))
	if err != nil {
		return err
	}
	toName, err := windows.UTF16PtrFromString(filepath.Join(dir, filepath.Base(to)))
	if err != nil {
		return err
	}
	attrs, err := windows.GetFileAttributes(toName)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return root.Rename(from, to)
	}
	if err != nil {
		return err
	}
	if attrs&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return fmt.Errorf("rooted replacement requires a regular destination")
	}
	return replaceFileWFlags(toName, fromName, replacefileIgnoreMergeErrors|replacefileIgnoreACLErrors)
}
