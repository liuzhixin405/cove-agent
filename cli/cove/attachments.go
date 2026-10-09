package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	_ "golang.org/x/image/webp"
)

// Image processing limits (align with upstream API best practices)
const (
	maxImageDim      = 1568 // max pixels on longest side
	maxFlashImageDim = 4096
	maxImageBytes    = 5 * 1024 * 1024  // 5MB target after compression
	maxRawImage      = 32 * 1024 * 1024 // 32MB raw file read limit
	jpegQuality      = 85
	minJPEGQuality   = 20
	// maxDecodePixels caps the decoded pixel count. The 32MB raw limit says
	// nothing about the decoded size: a highly compressible PNG a few hundred
	// KB on disk can declare 60000x60000, and image decoders allocate
	// width*height*bytes-per-pixel up front — so decoding it is an instant OOM.
	// The dimensions are read from the header and checked BEFORE decoding.
	maxDecodePixels = 64 << 20 // 67,108,864 pixels (~256MB as RGBA)
)

// nonVisionImageWarning is the part of the non-vision-model warning that
// shouldAutoSwitchToVision recognises. The two used to be written separately:
// the check looked for "fallback"/"vision" in a Chinese warning containing
// neither, so the switch the manual promises never happened.
const nonVisionImageWarning = "可能不支持图片视觉功能"

// attachmentTokenRE matches an @path that starts a word. The @ used to match
// anywhere, so "foo@bar.com" or "git@github.com:x" made cove look for a file
// named "bar.com" and fail the whole prompt.
var attachmentTokenRE = regexp.MustCompile(`(^|\s)@("[^"]+"|'[^']+'|\S+)`)

// buildUserMessage turns a typed line into the message to send: @path tokens
// naming existing files become attachments (see extractInlineAttachments),
// explicitPaths (--image/--file, /attach) are attached as well. An explicit
// path that cannot be read is an error; an @token is not (see
// inlineAttachmentWanted), it stays text and a warning says so.
func buildUserMessage(input, cwd string, explicitPaths []string, model string) (api.Message, []string, error) {
	var warnings []string
	cleaned, inlinePaths := extractInlineAttachments(input, func(raw string) bool {
		if inlineAttachmentWanted(cwd, raw) {
			return true
		}
		if looksLikeAttachmentPath(raw) {
			warnings = append(warnings, fmt.Sprintf("⚠ @%s 不是已存在的文件，已按普通文本发送（附加文件请检查路径，或用 /attach）", raw))
		}
		return false
	})
	allPaths := append([]string{}, explicitPaths...)
	allPaths = append(allPaths, inlinePaths...)

	msg := api.Message{Role: "user", Content: strings.TrimSpace(cleaned)}
	if len(allPaths) == 0 {
		return msg, warnings, nil
	}

	seen := map[string]bool{}
	for _, p := range allPaths {
		part, absPath, warn, err := buildAttachmentPart(cwd, p, model)
		if err != nil {
			return api.Message{}, nil, err
		}
		if warn != "" {
			warnings = append(warnings, warn)
		}
		if seen[absPath] {
			continue
		}
		seen[absPath] = true
		msg.Parts = append(msg.Parts, part)
	}
	return msg, warnings, nil
}

func handleAttachCommand(input, cwd string, attached *[]string) {
	argsText := strings.TrimSpace(strings.TrimPrefix(input, "/attach"))
	if argsText == "" || strings.EqualFold(argsText, "list") {
		printAttachmentList(*attached)
		return
	}

	args, err := splitQuotedFields(argsText)
	if err != nil {
		outf("附件命令解析失败: %v\n", err)
		return
	}
	if len(args) == 0 {
		printAttachmentList(*attached)
		return
	}

	switch strings.ToLower(args[0]) {
	case "list", "ls":
		printAttachmentList(*attached)
	case "clear":
		*attached = nil
		outln("已清空附件列表")
	case "remove", "rm":
		removeAttachment(args[1:], attached)
	case "add":
		addAttachments(args[1:], cwd, attached)
	default:
		addAttachments(args, cwd, attached)
	}
}

func addAttachments(paths []string, cwd string, attached *[]string) {
	if len(paths) == 0 {
		outln("用法: /attach <文件...> | /attach list | /attach remove <序号> | /attach clear")
		return
	}
	seen := map[string]bool{}
	for _, existing := range *attached {
		seen[existing] = true
	}
	added := 0
	for _, rawPath := range paths {
		absPath, err := normalizeAttachmentPath(cwd, rawPath)
		if err != nil {
			outf("跳过 %s: %v\n", rawPath, err)
			continue
		}
		if seen[absPath] {
			continue
		}
		*attached = append(*attached, absPath)
		seen[absPath] = true
		added++
	}
	outf("已挂载 %d 个附件，当前共 %d 个。\n", added, len(*attached))
	printAttachmentList(*attached)
}

func removeAttachment(args []string, attached *[]string) {
	if len(args) == 0 {
		outln("用法: /attach remove <序号>")
		return
	}
	idx, err := strconv.Atoi(args[0])
	if err != nil || idx < 1 || idx > len(*attached) {
		outf("无效附件序号: %s\n", args[0])
		return
	}
	removed := (*attached)[idx-1]
	*attached = append((*attached)[:idx-1], (*attached)[idx:]...)
	outf("已移除附件: %s\n", removed)
	printAttachmentList(*attached)
}

func printAttachmentList(paths []string) {
	if len(paths) == 0 {
		outln("当前没有挂载附件。用 /attach <文件...> 添加。")
		return
	}
	outln("当前挂载附件:")
	for i, p := range paths {
		outf("  %d. %s\n", i+1, p)
	}
}

func normalizeAttachmentPath(cwd, rawPath string) (string, error) {
	absPath := strings.TrimSpace(rawPath)
	absPath = strings.TrimPrefix(absPath, "@")
	absPath = strings.Trim(absPath, `"'`)
	if absPath == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(cwd, absPath)
	}
	absPath = filepath.Clean(absPath)
	st, err := os.Stat(absPath)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("路径是目录，不是文件")
	}
	return absPath, nil
}

func splitQuotedFields(input string) ([]string, error) {
	var fields []string
	var b strings.Builder
	var quote rune
	inToken := false
	for _, r := range input {
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			inToken = true
			continue
		}

		switch r {
		case '\'', '"':
			quote = r
			inToken = true
		case ' ', '\t', '\r', '\n':
			if inToken {
				fields = append(fields, b.String())
				b.Reset()
				inToken = false
			}
		default:
			b.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("引号未闭合")
	}
	if inToken {
		fields = append(fields, b.String())
	}
	return fields, nil
}

// inlineAttachmentWanted decides whether an @token names an attachment: only
// when it is an existing file. A token that looked like a path (a dot or a
// separator) used to be taken whatever it named, and a missing file failed
// the whole prompt with 读取附件失败 — for a package name like @babel/core or
// @types/node in a pasted log as much as for a typo. Such a token now stays
// text and buildUserMessage warns about it (looksLikeAttachmentPath), so a
// mistyped @logs/app.lgo is still reported. Bare words such as @Override or
// @dataclass are everyday text in coding prompts and pass silently.
func inlineAttachmentWanted(cwd, raw string) bool {
	p := raw
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// looksLikeAttachmentPath reports whether an @token that is not a file was
// probably meant as one (it has a dot or a separator), which is worth a
// warning, unlike a bare @word.
func looksLikeAttachmentPath(raw string) bool {
	return strings.ContainsAny(raw, `./\:`)
}

// extractInlineAttachments removes the @path tokens that want accepts from
// input and returns them; the others stay in the text unchanged.
//
// Only the token goes, with one space next to it. The rest of the text used
// to be rebuilt with strings.Join(strings.Fields(...), " ") as soon as one
// token was taken, which flattened the whole message onto one line: the
// newlines and indentation of a pasted code block or log were lost.
func extractInlineAttachments(input string, want func(raw string) bool) (string, []string) {
	matches := attachmentTokenRE.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return input, nil
	}
	paths := make([]string, 0, len(matches))
	var out strings.Builder
	last := 0
	taken := 0
	isBlank := func(s string) bool { return s == " " || s == "\t" }
	for _, m := range matches {
		// m[2]:m[3] is the whitespace before the @ ("" at the start),
		// m[4]:m[5] the path.
		before, end := input[m[2]:m[3]], m[1]
		raw := strings.TrimSpace(input[m[4]:m[5]])
		raw = strings.Trim(raw, `"'`)
		if raw == "" || (want != nil && !want(raw)) {
			continue
		}
		// "看 @a.txt 的内容" → "看 的内容": the space before goes. At the start
		// of the text or a line the newline stays, and the space after the
		// token goes instead.
		// When the space before already went with the previous token
		// ("@a @b x"), the one after goes.
		start := m[3]
		if isBlank(before) && m[2] >= last {
			start = m[2]
		} else if end < len(input) && isBlank(input[end:end+1]) {
			end++
		}
		if start < last {
			start = last
		}
		out.WriteString(input[last:start])
		paths = append(paths, raw)
		last = end
		taken++
	}
	if taken == 0 {
		return input, nil
	}
	out.WriteString(input[last:])
	return out.String(), paths
}

// buildAttachmentPart reads a file and creates an api.MessagePart.
// Returns (part, absPath, warning, error).
// warning is non-empty for non-fatal issues (e.g., non-vision model with image).
func buildAttachmentPart(cwd, rawPath, model string) (api.MessagePart, string, string, error) {
	absPath := strings.TrimSpace(rawPath)
	if absPath == "" {
		return api.MessagePart{}, "", "", fmt.Errorf("附件路径不能为空")
	}
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(cwd, absPath)
	}
	absPath = filepath.Clean(absPath)

	st, err := os.Stat(absPath)
	if err != nil {
		return api.MessagePart{}, "", "", fmt.Errorf("读取附件失败 %s: %w", rawPath, err)
	}
	if st.IsDir() {
		return api.MessagePart{}, "", "", fmt.Errorf("附件路径是目录，不是文件: %s", rawPath)
	}

	// Read file (cap at 32MB for image processing headroom)
	readLimit := int64(maxRawImage)
	if st.Size() > readLimit {
		return api.MessagePart{}, "", "", fmt.Errorf(
			"文件过大 (%.1fMB > %.0fMB 限制): %s\n  提示: 请先用图片工具缩小尺寸后再挂载",
			float64(st.Size())/(1024*1024), float64(readLimit)/(1024*1024), rawPath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return api.MessagePart{}, "", "", fmt.Errorf("读取附件失败 %s: %w", rawPath, err)
	}

	name := filepath.Base(absPath)
	mimeType := detectMimeType(absPath, data)

	if strings.HasPrefix(mimeType, "image/") {
		// Only a decodable raster image is a vision part. SVG, ICO, BMP,
		// TIFF and AVIF all carry an image/* MIME type by extension and used
		// to be refused as "unsupported image format", dropping the whole
		// message; they are sent as text (SVG) or binary attachments instead.
		if _, _, err := validatedImageConfig(data); err == nil {
			return buildImagePart(name, absPath, data, mimeType, model)
		}
	}

	return buildTextPart(name, absPath, data, mimeType)
}

// buildImagePart processes an image file and creates a vision API part.
func buildImagePart(name, absPath string, raw []byte, mimeType, model string) (api.MessagePart, string, string, error) {
	// Check if model supports vision
	warning := ""
	if model != "" && !api.IsVisionCapableModel(model) {
		warning = fmt.Sprintf("⚠ 当前模型 %s %s，将尝试切换视觉模型；无法切换时不会发送图片。建议使用 deepseek-flash / gpt-4o / claude-sonnet-4", model, nonVisionImageWarning)
	}

	originalDim := maxImageDim
	if strings.Contains(strings.ToLower(model), "deepseek") && api.IsVisionCapableModel(model) {
		originalDim = maxFlashImageDim
	}
	processed, finalMime, err := processImageWithLimit(raw, originalDim)
	if err != nil {
		return api.MessagePart{}, "", "", fmt.Errorf("处理图片失败 %s: %w", name, err)
	}

	// Final size check
	if len(processed) > maxImageBytes*2 {
		return api.MessagePart{}, "", "", fmt.Errorf(
			"图片压缩后仍然过大 (%.1fMB): %s\n  提示: 请减小图片尺寸后再挂载",
			float64(len(processed))/(1024*1024), name)
	}

	return api.MessagePart{
		Type:     "image",
		MimeType: finalMime,
		Data:     base64.StdEncoding.EncodeToString(processed),
		FileName: name,
	}, absPath, warning, nil
}

// buildTextPart creates a text/file attachment part.
func buildTextPart(name, absPath string, data []byte, mimeType string) (api.MessagePart, string, string, error) {
	const textLimit = 200 * 1024
	const binLimit = 96 * 1024
	if utf8.Valid(data) {
		body := string(data)
		truncated := ""
		if len(body) > textLimit {
			// Clip on a rune boundary. The data was just verified as valid
			// UTF-8, and a raw body[:textLimit] then re-broke it by cutting a
			// multi-byte rune in half — attaching any Chinese text file over
			// 200KB shipped invalid UTF-8 to the provider.
			body = textutil.ClipBytes(body, textLimit, "")
			truncated = "\n[内容已截断: 仅发送前 200KB]"
		}
		return api.MessagePart{
			Type:     "text",
			MimeType: mimeType,
			FileName: name,
			Text:     fmt.Sprintf("附件 %s (%s) 内容:\n```text\n%s\n```%s", name, mimeType, body, truncated),
		}, absPath, "", nil
	}

	payload := data
	truncated := ""
	if len(payload) > binLimit {
		payload = payload[:binLimit]
		truncated = " (已截断)"
	}
	encoded := base64.StdEncoding.EncodeToString(payload)
	return api.MessagePart{
		Type:     "text",
		MimeType: mimeType,
		FileName: name,
		Text:     fmt.Sprintf("附件 %s (%s) 为二进制文件，以下为 base64 片段%s:\n%s", name, mimeType, truncated, encoded),
	}, absPath, "", nil
}

// ---------------------------------------------------------------------------
// Image processing
// ---------------------------------------------------------------------------

func processImage(raw []byte) ([]byte, string, error) {
	return processImageWithLimit(raw, maxImageDim)
}

func processImageWithLimit(raw []byte, originalDim int) ([]byte, string, error) {
	cfg, format, err := validatedImageConfig(raw)
	if err != nil {
		return nil, "", err
	}
	if len(raw) <= maxImageBytes && cfg.Width <= originalDim && cfg.Height <= originalDim {
		mimeType := "image/" + format
		return raw, mimeType, nil
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode image: %w", err)
	}

	// Resize if needed
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	longest := w
	if h > w {
		longest = h
	}
	if longest > originalDim {
		// originalDim is the model's limit (4096 for Flash): a 3000px image
		// over the byte budget only needs re-encoding, not a 1568px resize.
		img = resizeImage(img, w, h, originalDim)
	}

	// Encode as JPEG with quality compression
	encoded, err := compressToSize(img, maxImageBytes, jpegQuality)
	if err != nil {
		return nil, "", fmt.Errorf("encode image: %w", err)
	}

	return encoded, "image/jpeg", nil
}

// decodeImage decodes PNG, JPEG, or GIF from raw bytes.
//
// The declared dimensions are checked from the header first. Decoding straight
// away let a decompression bomb — a small file declaring enormous dimensions —
// drive the decoder's up-front width*height allocation and take the process
// down with it, long before processImage's maxImageDim resize ever ran.
func decodeImage(raw []byte) (image.Image, error) {
	if _, _, err := validatedImageConfig(raw); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

func validatedImageConfig(raw []byte) (image.Config, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return image.Config{}, "", fmt.Errorf("unsupported image format (支持: PNG, JPEG, GIF, WebP): %w", err)
	}
	switch format {
	case "png", "jpeg", "gif", "webp":
	default:
		return image.Config{}, "", fmt.Errorf("unsupported image format %q (支持: PNG, JPEG, GIF, WebP)", format)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return image.Config{}, "", fmt.Errorf("image reports invalid dimensions %dx%d", cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodePixels {
		return image.Config{}, "", fmt.Errorf(
			"图片过大: %dx%d (%d 像素) 超过 %d 像素上限，请先缩小尺寸",
			cfg.Width, cfg.Height, int64(cfg.Width)*int64(cfg.Height), int64(maxDecodePixels))
	}
	return cfg, format, nil
}

// resizeImage scales an image down so its longest side <= maxDim,
// preserving aspect ratio. Uses bilinear interpolation.
func resizeImage(img image.Image, srcW, srcH, maxDim int) image.Image {
	// Calculate new dimensions
	newW, newH := srcW, srcH
	if srcW >= srcH && srcW > maxDim {
		newW = maxDim
		newH = int(float64(srcH) * float64(maxDim) / float64(srcW))
	} else if srcH > maxDim {
		newH = maxDim
		newW = int(float64(srcW) * float64(maxDim) / float64(srcH))
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	return bilinearResize(img, srcW, srcH, newW, newH)
}

// bilinearResize performs bilinear interpolation downscaling.
func bilinearResize(src image.Image, sw, sh, dw, dh int) image.Image {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	xs := float64(sw) / float64(dw)
	ys := float64(sh) / float64(dh)

	for dy := 0; dy < dh; dy++ {
		sy := float64(dy)*ys + 0.5
		syi := int(sy)
		syf := sy - float64(syi)
		if syi >= sh-1 {
			syi = sh - 2
			syf = 1.0
		}
		if syi < 0 {
			syi = 0
			syf = 0
		}

		for dx := 0; dx < dw; dx++ {
			sx := float64(dx)*xs + 0.5
			sxi := int(sx)
			sxf := sx - float64(sxi)
			if sxi >= sw-1 {
				sxi = sw - 2
				sxf = 1.0
			}
			if sxi < 0 {
				sxi = 0
				sxf = 0
			}

			// Sample 4 neighbors
			c00r, c00g, c00b, c00a := src.At(sxi, syi).RGBA()
			c10r, c10g, c10b, c10a := src.At(sxi+1, syi).RGBA()
			c01r, c01g, c01b, c01a := src.At(sxi, syi+1).RGBA()
			c11r, c11g, c11b, c11a := src.At(sxi+1, syi+1).RGBA()

			// Bilinear interpolation
			ixf := 65535.0 - float64(sxf*65535)
			iyf := 65535.0 - float64(syf*65535)
			sxf32 := float64(sxf * 65535)
			syf32 := float64(syf * 65535)

			r := uint8((float64(c00r)*ixf*iyf + float64(c10r)*sxf32*iyf + float64(c01r)*ixf*syf32 + float64(c11r)*sxf32*syf32) / (65535.0 * 65535.0) / 257.0)
			g := uint8((float64(c00g)*ixf*iyf + float64(c10g)*sxf32*iyf + float64(c01g)*ixf*syf32 + float64(c11g)*sxf32*syf32) / (65535.0 * 65535.0) / 257.0)
			b := uint8((float64(c00b)*ixf*iyf + float64(c10b)*sxf32*iyf + float64(c01b)*ixf*syf32 + float64(c11b)*sxf32*syf32) / (65535.0 * 65535.0) / 257.0)
			_ = c00a + c10a + c01a + c11a // suppress unused warning

			dst.SetRGBA(dx, dy, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}

	return dst
}

// compressToSize encodes img as JPEG, iteratively reducing quality until
// the output is under maxBytes (or quality reaches minJPEGQuality).
func compressToSize(img image.Image, maxBytes int, startQuality int) ([]byte, error) {
	quality := startQuality

	// Strip alpha channel for JPEG encoding
	rgb := stripAlpha(img)

	for quality >= minJPEGQuality {
		var buf bytes.Buffer
		err := jpeg.Encode(&buf, rgb, &jpeg.Options{Quality: quality})
		if err != nil {
			return nil, err
		}
		if buf.Len() <= maxBytes {
			return buf.Bytes(), nil
		}
		quality -= 15
	}

	// Return smallest version even if over limit
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgb, &jpeg.Options{Quality: minJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// stripAlpha creates an opaque RGBA image (JPEG doesn't support alpha).
func stripAlpha(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	dst := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			// r, g, b are always in [0, 0xffff] per image.Color.RGBA's contract,
			// so the >>8 shift always yields a value in [0, 255]: safe despite
			// the narrowing conversion.
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r >> 8), //nolint:gosec // bounded to 0-255 by the shift, see above
				G: uint8(g >> 8), //nolint:gosec // bounded to 0-255 by the shift, see above
				B: uint8(b >> 8), //nolint:gosec // bounded to 0-255 by the shift, see above
				A: 255,
			})
		}
	}
	return dst
}

func detectMimeType(path string, data []byte) string {
	m := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if m != "" {
		if idx := strings.Index(m, ";"); idx > 0 {
			return m[:idx]
		}
		return m
	}
	if len(data) > 0 {
		d := data
		if len(d) > 512 {
			d = d[:512]
		}
		return http.DetectContentType(d)
	}
	return "application/octet-stream"
}
