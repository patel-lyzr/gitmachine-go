package gitmachine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const credentialsFileName = "credentials.json"

// CloudCredential stores credentials for a single cloud provider account.
type CloudCredential struct {
	// Name is a user-friendly label (e.g. "personal-aws", "work-gcp").
	Name string `json:"name"`

	// Provider is the cloud type: "aws", "azure", "gcp".
	Provider string `json:"provider"`

	// Fields holds provider-specific credential key-value pairs.
	// AWS: access_key_id, secret_access_key, region
	// Azure: subscription_id, tenant_id, client_id, client_secret, region
	// GCP: project_id, credentials_file, region
	Fields map[string]string `json:"fields"`

	// Default marks this as the default credential for its provider.
	Default bool `json:"default,omitempty"`
}

// CredentialStore manages persistent cloud credentials on disk.
type CredentialStore struct {
	path        string
	Credentials []CloudCredential `json:"credentials"`
}

// NewCredentialStore loads or creates the credential store at ~/.gitmachine/credentials.json.
func NewCredentialStore() (*CredentialStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("get home dir: %w", err)
	}

	dir := filepath.Join(home, ".gitmachine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	cs := &CredentialStore{path: filepath.Join(dir, credentialsFileName)}
	_ = cs.load()
	return cs, nil
}

// Add adds a credential and saves. If it's the first for its provider, it's set as default.
func (cs *CredentialStore) Add(cred CloudCredential) error {
	// Check if this is the first for the provider.
	hasExisting := false
	for _, c := range cs.Credentials {
		if c.Provider == cred.Provider {
			hasExisting = true
			break
		}
	}
	if !hasExisting {
		cred.Default = true
	}

	cs.Credentials = append(cs.Credentials, cred)
	return cs.save()
}

// Remove removes a credential by name and saves.
func (cs *CredentialStore) Remove(name string) error {
	for i := range cs.Credentials {
		if cs.Credentials[i].Name == name {
			wasDefault := cs.Credentials[i].Default
			provider := cs.Credentials[i].Provider
			cs.Credentials = append(cs.Credentials[:i], cs.Credentials[i+1:]...)

			// If the removed one was default, make the first remaining one for that provider default.
			if wasDefault {
				for j := range cs.Credentials {
					if cs.Credentials[j].Provider == provider {
						cs.Credentials[j].Default = true
						break
					}
				}
			}
			return cs.save()
		}
	}
	return fmt.Errorf("credential %q not found", name)
}

// SetDefault sets a credential as the default for its provider.
func (cs *CredentialStore) SetDefault(name string) error {
	var target *CloudCredential
	for i := range cs.Credentials {
		if cs.Credentials[i].Name == name {
			target = &cs.Credentials[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("credential %q not found", name)
	}

	// Clear default for same provider, set this one.
	for i := range cs.Credentials {
		if cs.Credentials[i].Provider == target.Provider {
			cs.Credentials[i].Default = false
		}
	}
	target.Default = true
	return cs.save()
}

// GetDefault returns the default credential for a provider, or nil.
func (cs *CredentialStore) GetDefault(provider string) *CloudCredential {
	for i := range cs.Credentials {
		if cs.Credentials[i].Provider == provider && cs.Credentials[i].Default {
			return &cs.Credentials[i]
		}
	}
	// Fallback: return the first one for this provider.
	for i := range cs.Credentials {
		if cs.Credentials[i].Provider == provider {
			return &cs.Credentials[i]
		}
	}
	return nil
}

// Get returns a credential by name, or nil.
func (cs *CredentialStore) Get(name string) *CloudCredential {
	for i := range cs.Credentials {
		if cs.Credentials[i].Name == name {
			return &cs.Credentials[i]
		}
	}
	return nil
}

// ListByProvider returns all credentials for a given provider.
func (cs *CredentialStore) ListByProvider(provider string) []CloudCredential {
	var out []CloudCredential
	for _, c := range cs.Credentials {
		if c.Provider == provider {
			out = append(out, c)
		}
	}
	return out
}

func (cs *CredentialStore) load() error {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, cs)
}

func (cs *CredentialStore) save() error {
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.path, data, 0o600)
}
