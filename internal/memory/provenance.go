package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
)

// ProvenanceSource identifies the inputs of a memory write, not its truth.
type ProvenanceSource struct {
	Kind           string    `json:"kind"`
	SessionIDs     []string  `json:"session_ids,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	FirstMessage   int       `json:"first_message,omitempty"`
	LastMessage    int       `json:"last_message,omitempty"`
	TranscriptHash string    `json:"transcript_hash,omitempty"`
	Evidence       string    `json:"evidence,omitempty"`
	At             time.Time `json:"at"`
}

// Provenance binds source records to one exact revision of a memory.
type Provenance struct {
	Version     int                `json:"version"`
	ContentHash string             `json:"content_hash"`
	Sources     []ProvenanceSource `json:"sources"`
}

func provenanceHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func provenancePath(dir, name, content string) string {
	return filepath.Join(dir, ".provenance", provenanceHash(pathKey(name)), provenanceHash(content)+".json")
}

func readProvenance(dir, name, content string) (*Provenance, error) {
	data, err := os.ReadFile(provenancePath(dir, name, content))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record Provenance
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	if record.Version != 1 || record.ContentHash != provenanceHash(content) {
		return nil, fmt.Errorf("memory provenance version or content mismatch")
	}
	return &record, nil
}

// Provenance returns sources for the currently visible revision, if known.
// Legacy or externally edited content has no inferred source record.
func (s *Store) Provenance(name string) (*Provenance, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	for _, dir := range s.dirs {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return readProvenance(dir, name, string(data))
	}
	return nil, fmt.Errorf("memory %q not found", name)
}

func (s *Store) prepareProvenance(name, content string, source *ProvenanceSource, appendTo string) error {
	if source == nil {
		return nil
	}
	record := Provenance{Version: 1, ContentHash: provenanceHash(content)}
	if appendTo != "" {
		previous, err := s.Provenance(appendTo)
		if err != nil {
			return err
		}
		if previous != nil {
			record.Sources = append(record.Sources, previous.Sources...)
		} else {
			record.Sources = append(record.Sources, ProvenanceSource{Kind: "unknown", Evidence: "包含无来源记录的原有正文"})
		}
	}
	copySource := *source
	if copySource.At.IsZero() {
		copySource.At = time.Now()
	}
	record.Sources = append(record.Sources, copySource)
	if len(record.Sources) > 64 {
		record.Sources = record.Sources[len(record.Sources)-64:]
	}
	path := provenancePath(s.PrimaryDir(), name, content)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return fsatomic.WriteFile(path, data, 0600)
}

// SaveWithSource saves content only after its version-bound source is durable.
func (s *Store) SaveWithSource(name, content string, source ProvenanceSource) error {
	return s.save(name, content, &source, "")
}

// AppendWithSource carries forward known sources when existing content is kept.
func (s *Store) AppendWithSource(name, content string, source ProvenanceSource) (string, error) {
	return s.append(name, content, &source)
}
