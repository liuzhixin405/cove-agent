package main

import (
	"context"
	"fmt"
	"os"

	"golang.design/x/clipboard"
)

func readClipboardImage(ctx context.Context) ([]byte, error) {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" {
		return nil, fmt.Errorf("SSH cannot access your local image clipboard; use @path or /attach")
	}
	if err := clipboard.Init(); err != nil {
		return nil, fmt.Errorf("image clipboard unavailable: %w", err)
	}
	raw, err := clipboard.Read(ctx, clipboard.Register("image/png"))
	if err != nil {
		return nil, fmt.Errorf("cannot paste PNG image; capture with Win+Shift+S or use @path: %w", err)
	}
	return raw, nil
}
