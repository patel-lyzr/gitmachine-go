package gitmachine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "nodes.json"

// NodeRecord persists metadata about a managed cloud node.
type NodeRecord struct {
	ID           string            `json:"id"`
	Provider     string            `json:"provider"`
	InstanceType string            `json:"instance_type"`
	Region       string            `json:"region"`
	PublicIP     string            `json:"public_ip"`
	SSHUser      string            `json:"ssh_user"`
	SSHKeyPath   string            `json:"ssh_key_path"`
	Status       string            `json:"status"`
	CreatedAt    time.Time         `json:"created_at"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// NodeState manages persistent node records on disk.
type NodeState struct {
	path  string
	Nodes []NodeRecord `json:"nodes"`
}

// NewNodeState loads or creates the state file at ~/.gitmachine/nodes.json.
func NewNodeState() (*NodeState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	dir := filepath.Join(home, ".gitmachine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	s := &NodeState{path: filepath.Join(dir, stateFileName)}
	_ = s.load()
	return s, nil
}

// Add adds a node record and saves.
func (s *NodeState) Add(node NodeRecord) error {
	s.Nodes = append(s.Nodes, node)
	return s.save()
}

// Update updates a node record by ID and saves.
func (s *NodeState) Update(id string, fn func(*NodeRecord)) error {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			fn(&s.Nodes[i])
			return s.save()
		}
	}
	return fmt.Errorf("node %s not found", id)
}

// Remove removes a node record by ID and saves.
func (s *NodeState) Remove(id string) error {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			s.Nodes = append(s.Nodes[:i], s.Nodes[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("node %s not found", id)
}

// Find returns a node record by ID or nil.
func (s *NodeState) Find(id string) *NodeRecord {
	for i := range s.Nodes {
		if s.Nodes[i].ID == id {
			return &s.Nodes[i]
		}
	}
	return nil
}

// SaveKey writes a private key to ~/.gitmachine/keys/<id>.pem and returns the path.
func (s *NodeState) SaveKey(id string, privateKeyPEM string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	keysDir := filepath.Join(home, ".gitmachine", "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return "", err
	}
	keyPath := filepath.Join(keysDir, id+".pem")
	if err := os.WriteFile(keyPath, []byte(privateKeyPEM), 0o600); err != nil {
		return "", err
	}
	return keyPath, nil
}

// RemoveKey deletes the key file for a node.
func (s *NodeState) RemoveKey(id string) {
	home, _ := os.UserHomeDir()
	keyPath := filepath.Join(home, ".gitmachine", "keys", id+".pem")
	_ = os.Remove(keyPath)
}

func (s *NodeState) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, s)
}

func (s *NodeState) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}
