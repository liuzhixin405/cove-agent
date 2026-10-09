//go:build !windows

package main

import (
	"context"
	"fmt"
)

func readClipboardImage(context.Context) ([]byte, error) {
	return nil, fmt.Errorf("image clipboard is currently supported on Windows; use @path or /attach")
}
