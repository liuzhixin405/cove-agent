package config

import (
	"path/filepath"
	"testing"
)

// Deleting the active profile writes its values to the top level. The
// provider used to be merged field by field against a baseline Save had just
// dropped, so a field the profile did not set (the old top-level base_url,
// image_files_api) survived next to the profile's name and key: the new key
// was sent to the old endpoint on the next start.
func TestDeletingTheActiveProfileReplacesTheProviderWhole(t *testing.T) {
	global, _ := isolate(t)
	writeFile(t, filepath.Join(global, "config.json"),
		`{"provider":{"name":"openai-compatible","api_key":"local-key-123456","base_url":"http://127.0.0.1:8080/v1","image_files_api":true},`+
			`"active_profile":"ds","profiles":{"ds":{"provider":{"name":"deepseek","api_key":"sk-ds-1234567890"}}}}`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.Name != "deepseek" || cfg.Provider.BaseURL != "" || cfg.Provider.ImageFilesAPI != nil {
		t.Fatalf("loaded provider = %+v, want the profile's", cfg.Provider)
	}
	delete(cfg.Profiles, "ds")
	cfg.ActiveProfile = ""
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.Provider.Name != "deepseek" || again.Provider.APIKey != "sk-ds-1234567890" {
		t.Fatalf("provider after delete = %+v, want deepseek with the profile's key", again.Provider)
	}
	if again.Provider.BaseURL != "" || again.Provider.ImageFilesAPI != nil {
		t.Fatalf("stale top-level provider fields survived: base_url=%q image_files_api=%v", again.Provider.BaseURL, again.Provider.ImageFilesAPI)
	}
}
