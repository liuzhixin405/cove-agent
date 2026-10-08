package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

type WriteTool struct{ baseTool }

func NewWriteTool() Tool {
	return &WriteTool{baseTool{def: Def{
		Name: "write", Aliases: []string{"Write"},
		Description: "Write a file to the local filesystem. Creates parent directories if needed. Overwrites existing files.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to write the file"},
				"content":{"type":"string","description":"The content to write"}
			},
			"required":["filePath","content"]
		}`),
		IsReadOnly: false, IsConcurrencySafe: false, UserFacingName: "Write",
	}}}
}

func (t *WriteTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	path, _ := input["filePath"].(string)
	content, _ := input["content"].(string)

	// Fallback: models sometimes use alternative key names
	if path == "" {
		for _, alt := range []string{"file_path", "path", "filepath", "file"} {
			if v, ok := input[alt].(string); ok && v != "" {
				path = v
				break
			}
		}
	}
	if content == "" {
		if v, ok := input["text"].(string); ok && v != "" {
			content = v
		}
	}

	if path == "" {
		// Log the full input keys for debugging
		keys := make([]string, 0, len(input))
		for k := range input {
			keys = append(keys, k)
		}
		return Result{Data: fmt.Sprintf("Error: filePath required (received keys: %v)", keys), IsError: true}, nil
	}
	path, err := resolvePathInCwd(path, tctx, true)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return Result{Data: "Error: mkdir: " + err.Error(), IsError: true}, nil
	}
	files := fileTracker(tctx)
	var legacy *legacyText
	if existing, err := os.ReadFile(path); err == nil {
		// Creating a file needs no prior read; replacing one does.
		if err := files.Check(path, existing); err != nil {
			return Result{Data: "Error: " + err.Error() + ", then write it again.", IsError: true}, nil
		}
		existingText := string(existing)
		if !utf8.Valid(existing) {
			if lt, ok := decodeGBK(existing); ok {
				legacy, existingText = &lt, lt.text
			}
		}
		content = matchExistingFile(content, existingText)
	}
	out := []byte(content)
	if legacy != nil {
		// Keep a GBK file GBK, as line endings and the BOM are kept: other
		// tools reading it (the compiler, the IDE, the user's editor) assume
		// the encoding it already has.
		var err error
		if out, err = legacy.encode(content); err != nil {
			return legacy.encodeFailure(path, "content", err), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	if err := replaceFile(path, out); err != nil {
		return Result{Data: "Error: write: " + err.Error(), IsError: true}, nil
	}
	files.Record(path, out)

	lines := strings.Count(content, "\n") + 1
	return Result{Data: "Wrote " + strconv.Itoa(len(out)) + " bytes (" + strconv.Itoa(lines) + " lines) to " + path}, nil
}

func (t *WriteTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	switch tctx.PermissionMode {
	case "bypass", "auto":
		return Allowed("mode: " + tctx.PermissionMode)
	case "plan":
		return Denied("plan mode: write not allowed")
	}
	return Asked("write modifies filesystem")
}
