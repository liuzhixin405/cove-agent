//go:build windows

package fsatomic

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procReplaceFileW = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

const (
	replacefileIgnoreMergeErrors = 0x2
	replacefileIgnoreACLErrors   = 0x4
)

// keptAttributes are the attributes a replaced file hands on to its new
// content; the rest (directory, reparse point, compressed...) are not
// settable with SetFileAttributes or describe the old file's storage.
const keptAttributes = windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_HIDDEN |
	windows.FILE_ATTRIBUTE_SYSTEM | windows.FILE_ATTRIBUTE_ARCHIVE |
	windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED | windows.FILE_ATTRIBUTE_TEMPORARY

// platformRename moves the finished temp file onto path.
//
// A plain rename replaced the destination with the temp file, and so with the
// temp file's metadata: a hidden .env came back visible, and the old file's
// ACL, alternate data streams and creation time were gone. When path exists,
// ReplaceFileW swaps the content in while keeping those (attributes, DACL,
// named streams, creation time, short name). Where it cannot (some network
// shares and non-NTFS volumes), the rename is used and at least the
// attributes are put back; the ACL and streams are lost in that case.
func platformRename(from, to string) error {
	to16, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return os.Rename(from, to)
	}
	attrs, err := windows.GetFileAttributes(to16)
	if err != nil || attrs&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		// Nothing to keep (a new file), or not a plain file: rename as before.
		return os.Rename(from, to)
	}
	from16, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return os.Rename(from, to)
	}
	if err := replaceFileW(to16, from16); err == nil {
		return nil
	} else if isRetryableRename(err) && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The destination is open elsewhere; the caller retries.
		return err
	}
	// ERROR_UNABLE_TO_MOVE_REPLACEMENT (no backup file given) leaves the
	// destination removed and the temp file in place, so the rename below
	// also finishes that case.
	return renameKeepingAttributes(from, to, attrs)
}

func replaceFileW(replaced, replacement *uint16) error {
	return replaceFileWFlags(replaced, replacement, replacefileIgnoreMergeErrors|replacefileIgnoreACLErrors)
}

func replaceFileWFlags(replaced, replacement *uint16, flags uintptr) error {
	if err := procReplaceFileW.Find(); err != nil {
		return err
	}
	r, _, e := procReplaceFileW.Call(
		uintptr(unsafe.Pointer(replaced)),
		uintptr(unsafe.Pointer(replacement)),
		0,
		flags,
		0, 0)
	if r == 0 {
		if e == nil || errors.Is(e, windows.ERROR_SUCCESS) {
			return windows.ERROR_GEN_FAILURE
		}
		return e
	}
	return nil
}

// renameKeepingAttributes renames from onto to and re-applies attrs, the
// attributes to had before.
func renameKeepingAttributes(from, to string, attrs uint32) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	if keep := attrs & keptAttributes; keep != 0 {
		if to16, err := windows.UTF16PtrFromString(to); err == nil {
			// Best effort: the content is already in place, and failing the
			// write over an attribute would report a successful write as lost.
			_ = windows.SetFileAttributes(to16, keep)
		}
	}
	return nil
}
