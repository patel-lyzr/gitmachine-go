package gitmachine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const sandboxStateFileName = "sandboxes.json"

// SandboxRecord persists metadata about a managed sandbox (Docker or E2B).
type SandboxRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider,omitempty"` // "docker" (default) or "e2b"
	NodeID    string    `json:"node_id,omitempty"`
	Image     string    `json:"image"`
	CPUs      string    `json:"cpus,omitempty"`
	Memory    string    `json:"memory,omitempty"`
	DiskSize  string        `json:"disk_size,omitempty"`
	Ports     []PortMapping `json:"ports,omitempty"`
	Status    string        `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// SandboxState manages persistent sandbox records on disk.
type SandboxState struct {
	path      string
	Sandboxes []SandboxRecord `json:"sandboxes"`
}

// NewSandboxState loads or creates the state file at ~/.gitmachine/sandboxes.json.
func NewSandboxState() (*SandboxState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	dir := filepath.Join(home, ".gitmachine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	s := &SandboxState{path: filepath.Join(dir, sandboxStateFileName)}
	_ = s.load()
	return s, nil
}

// Add adds a sandbox record and saves.
func (s *SandboxState) Add(rec SandboxRecord) error {
	s.Sandboxes = append(s.Sandboxes, rec)
	return s.save()
}

// Update updates a sandbox record by ID and saves.
func (s *SandboxState) Update(id string, fn func(*SandboxRecord)) error {
	for i := range s.Sandboxes {
		if s.Sandboxes[i].ID == id {
			fn(&s.Sandboxes[i])
			return s.save()
		}
	}
	return fmt.Errorf("sandbox %s not found", id)
}

// Remove removes a sandbox record by ID and saves.
func (s *SandboxState) Remove(id string) error {
	for i := range s.Sandboxes {
		if s.Sandboxes[i].ID == id {
			s.Sandboxes = append(s.Sandboxes[:i], s.Sandboxes[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("sandbox %s not found", id)
}

// Find returns a sandbox record by ID or nil.
func (s *SandboxState) Find(id string) *SandboxRecord {
	for i := range s.Sandboxes {
		if s.Sandboxes[i].ID == id {
			return &s.Sandboxes[i]
		}
	}
	return nil
}

// FindByPrefix returns a sandbox matching the given ID or name prefix, or nil if ambiguous/not found.
func (s *SandboxState) FindByPrefix(prefix string) *SandboxRecord {
	var match *SandboxRecord
	for i := range s.Sandboxes {
		if strings.HasPrefix(s.Sandboxes[i].ID, prefix) || strings.HasPrefix(s.Sandboxes[i].Name, prefix) {
			if match != nil {
				return nil // ambiguous
			}
			match = &s.Sandboxes[i]
		}
	}
	return match
}

// ForNode returns all sandbox records belonging to a given node.
func (s *SandboxState) ForNode(nodeID string) []SandboxRecord {
	var out []SandboxRecord
	for _, r := range s.Sandboxes {
		if r.NodeID == nodeID {
			out = append(out, r)
		}
	}
	return out
}

// RemoveForNode removes all sandbox records for a given node and saves.
func (s *SandboxState) RemoveForNode(nodeID string) error {
	var kept []SandboxRecord
	for _, r := range s.Sandboxes {
		if r.NodeID != nodeID {
			kept = append(kept, r)
		}
	}
	s.Sandboxes = kept
	return s.save()
}

func (s *SandboxState) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, s)
}

func (s *SandboxState) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}
