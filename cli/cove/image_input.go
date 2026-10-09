package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type imageInput struct {
	paths     *[]string
	clipboard func(context.Context) ([]byte, error)
	tempDir   string
	owned     map[string]bool
	metadata  map[string]string
}

func newImageInput(paths *[]string) *imageInput {
	return &imageInput{paths: paths, clipboard: readClipboardImage, owned: make(map[string]bool), metadata: make(map[string]string)}
}

func (input *imageInput) paste(text, cwd string) bool {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 64*1024 {
		return false
	}
	paths := []string{text}
	if info, err := imagePathMetadata(cwd, text); err == nil {
		absolute, err := normalizeAttachmentPath(cwd, text)
		if err != nil {
			return false
		}
		input.add(absolute, info)
		return true
	}
	var parseErr error
	paths, parseErr = splitQuotedFields(text)
	if parseErr != nil || len(paths) == 0 || len(paths) > 32 {
		return false
	}
	resolved := make([]string, 0, len(paths))
	metadata := make(map[string]string)
	for _, path := range paths {
		info, err := imagePathMetadata(cwd, path)
		if err != nil {
			return false
		}
		absolute, err := normalizeAttachmentPath(cwd, path)
		if err != nil {
			return false
		}
		resolved = append(resolved, absolute)
		metadata[absolute] = info
	}
	for _, path := range resolved {
		input.add(path, metadata[path])
	}
	return true
}

func imagePathMetadata(cwd, path string) (string, error) {
	abs, err := normalizeAttachmentPath(cwd, path)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return "", fmt.Errorf("not an image path")
	}
	stat, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxRawImage {
		return "", fmt.Errorf("invalid image file size or type")
	}
	file, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer file.Close()
	stat, err = file.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() > maxRawImage {
		return "", fmt.Errorf("invalid image file size or type")
	}
	cfg, format, err := image.DecodeConfig(io.LimitReader(file, 1024*1024))
	if err != nil {
		return "", err
	}
	if format != "png" && format != "jpeg" && format != "gif" && format != "webp" {
		return "", fmt.Errorf("unsupported image format")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxDecodePixels/cfg.Height {
		return "", fmt.Errorf("unsafe image dimensions")
	}
	return imageMetadata(abs, cfg, stat.Size()), nil
}

func imageMetadata(path string, cfg image.Config, size int64) string {
	return fmt.Sprintf("%q %dx%d %.1f KB", filepath.Base(path), cfg.Width, cfg.Height, float64(size)/1024)
}

func (input *imageInput) add(path, metadata string) {
	input.metadata[path] = metadata
	for _, existing := range *input.paths {
		if existing == path {
			return
		}
	}
	*input.paths = append(*input.paths, path)
}

func (input *imageInput) pasteClipboard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := input.clipboard(ctx)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxRawImage {
		return fmt.Errorf("clipboard image is empty or larger than 32 MiB")
	}
	cfg, format, err := validatedImageConfig(raw)
	if err != nil {
		return err
	}
	if format != "png" {
		return fmt.Errorf("clipboard image must be PNG")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if input.tempDir == "" {
		input.tempDir, err = os.MkdirTemp("", "cove-clipboard-")
		if err != nil {
			return err
		}
	}
	digest := sha256.Sum256(raw)
	path := filepath.Join(input.tempDir, fmt.Sprintf("clipboard-%x.png", digest[:8]))
	if !input.owned[path] {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return err
		}
		input.owned[path] = true
	} else {
		stat, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !stat.Mode().IsRegular() || stat.Size() != int64(len(raw)) {
			return fmt.Errorf("clipboard image file changed")
		}
	}
	input.add(path, imageMetadata(path, cfg, int64(len(raw))))
	return nil
}

func (input *imageInput) summary() string {
	if len(*input.paths) == 0 {
		return ""
	}
	var parts []string
	for index, path := range *input.paths {
		if index == 4 {
			parts = append(parts, "...")
			break
		}
		info, ok := input.metadata[path]
		if !ok {
			info, _ = imagePathMetadata("", path)
			if info == "" {
				info = fmt.Sprintf("%q", filepath.Base(path))
			}
			input.metadata[path] = info
		}
		parts = append(parts, fmt.Sprintf("#%d %s", index+1, info))
	}
	return fmt.Sprintf("附件 (%d): %s", len(*input.paths), strings.Join(parts, " | "))
}

func (input *imageInput) prune() {
	active := make(map[string]bool)
	for _, path := range *input.paths {
		active[path] = true
	}
	for path := range input.owned {
		if !active[path] {
			if err := os.Remove(path); err == nil || os.IsNotExist(err) {
				delete(input.owned, path)
			}
		}
	}
	for path := range input.metadata {
		if !active[path] {
			delete(input.metadata, path)
		}
	}
}

func (input *imageInput) close() {
	if input.tempDir != "" {
		_ = os.RemoveAll(input.tempDir)
	}
}
