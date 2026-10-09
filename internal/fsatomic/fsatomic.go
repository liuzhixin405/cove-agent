// Package fsatomic provides crash-safe file replacement.
//
// The naive os.WriteFile truncates the destination before writing, so a crash,
// a full disk, or two writers racing leave the file half-written — and for the
// persisted state this project keeps (memory entries, session records, notes),
// a truncated file is worse than a stale one. WriteFile here writes to a
// temporary file in the same directory and renames it into place, which is
// atomic within a filesystem on both POSIX and Windows.
package fsatomic

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// tempPrefix marks the in-progress files WriteFile creates.
//
// The temp file lives in the destination directory (it has to, so the rename
// stays on one filesystem), which means any code that enumerates that
// directory can see it. If the process dies between create and rename the file
// is left behind — and directories like ~/.cove/memory are read back wholesale,
// so a leftover would be loaded forever as a bogus memory entry. Readers must
// skip names matching IsTempName.
const tempPrefix = ".cove-tmp-"

// IsTempName reports whether name is an in-progress file created by WriteFile.
// Every directory scan over a location this package writes to must skip these.
func IsTempName(name string) bool {
	return strings.HasPrefix(filepath.Base(name), tempPrefix)
}

const (
	// maxNameBytes is the file name length every common file system caps at
	// (NTFS, ext4, APFS, tmpfs: 255 bytes, or UTF-16 units on NTFS).
	maxNameBytes = 255
	// tempRandomLen is the longest suffix os.CreateTemp appends to a pattern
	// (a uint32 in decimal).
	tempRandomLen = 10
	// tempHashLen is the hex digits of the name hash a shortened pattern
	// carries so that two long names sharing a head get distinct patterns.
	tempHashLen = 16
)

// tempPattern returns the os.CreateTemp pattern for the temp file that
// replaces base.
//
// The pattern used to be tempPrefix+base+".", 11 bytes more than the name
// itself, plus the random suffix CreateTemp adds. A 250-byte name, which
// os.WriteFile handled, therefore produced a 271-byte temp name and CreateTemp
// failed ("filename syntax is incorrect" on Windows, ENAMETOOLONG elsewhere),
// so write and edit refused files they used to write. A name too long for that
// is cut on a rune boundary and given a hash of the full name instead.
func tempPattern(base string) string {
	const overhead = len(tempPrefix) + 1 + tempRandomLen // prefix, the "." and the random digits
	if len(base)+overhead <= maxNameBytes {
		return tempPrefix + base + "."
	}
	sum := sha256.Sum256([]byte(base))
	hash := hex.EncodeToString(sum[:])[:tempHashLen]
	keep := maxNameBytes - overhead - len(hash) - 1 // "-" between head and hash
	for keep > 0 && !utf8.RuneStart(base[keep]) {
		keep--
	}
	return tempPrefix + base[:keep] + "-" + hash + "."
}

// ErrStreamName is returned on Windows for a path whose file name contains
// ':', which names an NTFS alternate data stream rather than a file: an atomic
// replace by rename cannot write one.
var ErrStreamName = errors.New("file name contains ':' (an NTFS alternate data stream), which cannot be replaced atomically")

// WriteFile atomically replaces path with data.
//
// The temporary file is created alongside the destination (never in the system
// temp dir) so the final rename stays within one filesystem. On any failure the
// temporary file is removed and the original destination is left untouched.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	// On Windows a ':' in the base name (the drive letter is not part of it)
	// names an NTFS alternate data stream. The temp pattern carries the name,
	// so CreateTemp made the file .cove-tmp-<head> plus a stream on it; the
	// rename then failed and the cleanup removed only the stream, leaving an
	// empty .cove-tmp-<head> behind for good. Refuse before creating anything.
	if runtime.GOOS == "windows" && strings.Contains(filepath.Base(path), ":") {
		return fmt.Errorf("write %s: %w", path, ErrStreamName)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tempPattern(filepath.Base(path)))
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	// Flush to disk before the rename: without this the rename can land while
	// the data blocks are still buffered, which after a power loss yields a
	// correctly-named but empty file.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	// CreateTemp always uses 0600; apply the caller's mode explicitly.
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod temp for %s: %w", path, err)
	}
	if err := renameWithRetry(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename temp onto %s: %w", path, err)
	}
	return nil
}

// WriteFileRoot atomically replaces a relative path without escaping root.
func WriteFileRoot(root *os.Root, path string, data []byte, perm os.FileMode) error {
	if root == nil {
		return fmt.Errorf("atomic write requires a root")
	}
	if runtime.GOOS == "windows" && strings.Contains(filepath.Base(path), ":") {
		return ErrStreamName
	}
	tmpName := filepath.Join(filepath.Dir(path), tempPrefix+rand.Text())
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = root.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	err = renameRootFile(root, tmpName, path)
	for _, delay := range renameBackoff {
		if err == nil || !retryableRename(err) {
			break
		}
		sleep(delay)
		err = renameRootFile(root, tmpName, path)
	}
	if err != nil {
		return err
	}
	// The mode goes on after the replacement (the temporary file is 0600):
	// a read-only temporary file (perm 0444) made ReplaceFileW fail on
	// Windows with access denied, and a 0755 script restored by /undo must
	// keep its executable bit on POSIX.
	return root.Chmod(path, perm)
}

// renameBackoff is the wait before each retry of a failed rename.
var renameBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}

// Seams for tests.
var (
	rename          = platformRename
	retryableRename = isRetryableRename
	sleep           = time.Sleep
)

// renameWithRetry renames from onto to, retrying a transient failure.
//
// On Windows, replacing a file fails with a sharing violation or access
// denied while another process (a second cove, an editor, an antivirus
// scanner) has the destination open. That lasts milliseconds, and giving up
// on it turned a routine write of a shared file such as the session index
// into an error.
func renameWithRetry(from, to string) error {
	err := rename(from, to)
	for _, d := range renameBackoff {
		if err == nil || !retryableRename(err) {
			return err
		}
		sleep(d)
		err = rename(from, to)
	}
	return err
}
