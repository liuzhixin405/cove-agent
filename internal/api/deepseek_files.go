package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

type imageFileKey struct {
	Account [sha256.Size]byte
	Content [sha256.Size]byte
}

type imageFileEntry struct {
	ID      string
	Expires time.Time
}

type deepseekImageFiles struct {
	gate    chan struct{}
	entries map[imageFileKey]imageFileEntry
}

func (provider *openAICompatProvider) imageEndpoint(client *http.Client, body oaiReq) endpoint {
	result := provider.endpoint(client)
	if provider.imageFiles == nil || !IsVisionCapableModel(body.Model) || !strings.Contains(strings.ToLower(body.Model), "deepseek") {
		return result
	}
	result.prepare = func(ctx context.Context, original []byte, key string) ([]byte, error) {
		prepared := body
		prepared.Messages = append([]oaiMsg(nil), body.Messages...)
		failed := make(map[imageFileKey]bool)
		changed := false
		for index, message := range body.Messages {
			blocks, ok := message.Content.([]map[string]any)
			if message.Role != "user" || !ok {
				continue
			}
			converted := append([]map[string]any(nil), blocks...)
			for blockIndex, block := range blocks {
				if block["type"] != "image_url" {
					continue
				}
				imageURL, _ := block["image_url"].(map[string]any)
				url, _ := imageURL["url"].(string)
				raw, mediaType, err := inlineImageData(url)
				if err != nil {
					continue
				}
				cacheKey := imageFileKey{Account: sha256.Sum256([]byte(key)), Content: sha256.Sum256(raw)}
				if failed[cacheKey] {
					continue
				}
				fileID, err := provider.imageFile(ctx, key, raw, mediaType, cacheKey)
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					failed[cacheKey] = true
					log.Warnf("DeepSeek Files API upload unavailable; using inline image: %v", err)
					continue
				}
				converted[blockIndex] = map[string]any{"type": "file", "file_id": fileID}
				changed = true
			}
			prepared.Messages[index].Content = converted
		}
		if !changed {
			return original, nil
		}
		return json.Marshal(prepared)
	}
	return result
}

func inlineImageData(url string) ([]byte, string, error) {
	const maxBytes = 64 << 20
	parts := strings.SplitN(url, ";base64,", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "data:") {
		return nil, "", fmt.Errorf("not an inline image")
	}
	mediaType := strings.TrimPrefix(parts[0], "data:")
	switch mediaType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return nil, "", fmt.Errorf("unsupported image type")
	}
	if len(parts[1]) > base64.StdEncoding.EncodedLen(maxBytes) {
		return nil, "", fmt.Errorf("image exceeds upload limit")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 || len(raw) > maxBytes {
		return nil, "", fmt.Errorf("invalid inline image")
	}
	return raw, mediaType, nil
}

func (provider *openAICompatProvider) imageFile(ctx context.Context, key string, raw []byte, mediaType string, cacheKey imageFileKey) (string, error) {
	cache := provider.imageFiles
	select {
	case cache.gate <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-cache.gate }()
	now := time.Now()
	if entry, ok := cache.entries[cacheKey]; ok && now.Add(30*time.Second).Before(entry.Expires) {
		return entry.ID, nil
	}
	id, expires, err := provider.uploadImageFile(ctx, key, raw, mediaType, cacheKey.Content)
	if err != nil {
		return "", err
	}
	for storedKey, entry := range cache.entries {
		if !now.Add(30 * time.Second).Before(entry.Expires) {
			delete(cache.entries, storedKey)
		}
	}
	if len(cache.entries) >= 256 {
		var oldestKey imageFileKey
		var oldest time.Time
		for storedKey, entry := range cache.entries {
			if oldest.IsZero() || entry.Expires.Before(oldest) {
				oldestKey, oldest = storedKey, entry.Expires
			}
		}
		delete(cache.entries, oldestKey)
	}
	cache.entries[cacheKey] = imageFileEntry{ID: id, Expires: expires}
	return id, nil
}

func (provider *openAICompatProvider) uploadImageFile(ctx context.Context, key string, raw []byte, mediaType string, digest [sha256.Size]byte) (string, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	started := time.Now()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for field, value := range map[string]string{"purpose": "user_data", "expires_after[anchor]": "created_at", "expires_after[seconds]": "3600"} {
		if err := writer.WriteField(field, value); err != nil {
			return "", time.Time{}, fmt.Errorf("create upload form")
		}
	}
	header := make(textproto.MIMEHeader)
	filename := fmt.Sprintf("%x.%s", digest[:8], strings.TrimPrefix(mediaType, "image/"))
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	header.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create upload file part")
	}
	if _, err := part.Write(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("write upload file part")
	}
	if err := writer.Close(); err != nil {
		return "", time.Time{}, fmt.Errorf("finish upload form")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(provider.baseURL, "/v1")+"/files", &buffer)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create image upload request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	client := *provider.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("image upload transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", time.Time{}, fmt.Errorf("image upload returned HTTP %d", response.StatusCode)
	}
	var uploaded struct {
		ID        string `json:"id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&uploaded); err != nil || !strings.HasPrefix(uploaded.ID, "file-api-") || len(uploaded.ID) > 256 {
		return "", time.Time{}, fmt.Errorf("invalid image upload response")
	}
	expires := started.Add(time.Hour)
	if uploaded.ExpiresAt > 0 && time.Unix(uploaded.ExpiresAt, 0).Before(expires) {
		expires = time.Unix(uploaded.ExpiresAt, 0)
	}
	if !time.Now().Before(expires) {
		return "", time.Time{}, fmt.Errorf("uploaded image is already expired")
	}
	return uploaded.ID, expires, nil
}
