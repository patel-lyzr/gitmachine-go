package gitmachine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	e2bAPIBase     = "https://api.e2b.dev/v1"
	defaultTimeout = 300 // seconds
)

// E2BMachine implements Machine using the E2B sandbox REST API.
type E2BMachine struct {
	mu        sync.Mutex
	sandboxID string
	state     MachineState
	apiKey    string
	template  string
	timeout   int
	envs      map[string]string
	metadata  map[string]string
	client    *http.Client

	// clientID is the sandbox's hostname for envd API calls.
	clientID string
}

// NewE2BMachine creates a new E2BMachine with the given configuration.
// The machine starts in the Idle state — call Start() to create the sandbox.
func NewE2BMachine(config *E2BMachineConfig) *E2BMachine {
	apiKey := ""
	template := "base"
	timeout := defaultTimeout
	var envs map[string]string
	var metadata map[string]string

	if config != nil {
		apiKey = config.APIKey
		if config.Template != "" {
			template = config.Template
		}
		if config.Timeout > 0 {
			timeout = config.Timeout
		}
		envs = config.Envs
		metadata = config.Metadata
	}

	if apiKey == "" {
		apiKey = os.Getenv("E2B_API_KEY")
	}

	return &E2BMachine{
		state:    StateIdle,
		apiKey:   apiKey,
		template: template,
		timeout:  timeout,
		envs:     envs,
		metadata: metadata,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ConnectE2BMachine connects to an existing E2B sandbox by its ID and returns
// an E2BMachine in the Running state.
func ConnectE2BMachine(ctx context.Context, sandboxID string, config *E2BMachineConfig) (*E2BMachine, error) {
	m := NewE2BMachine(config)
	m.sandboxID = sandboxID
	m.clientID = sandboxID

	// Verify the sandbox exists by fetching its info.
	_, err := m.GetInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to sandbox %s: %w", sandboxID, err)
	}

	m.state = StateRunning
	return m, nil
}

// ID returns the sandbox ID, or empty string if not started.
func (m *E2BMachine) ID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxID
}

// State returns the current machine state.
func (m *E2BMachine) State() MachineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Start creates a new E2B sandbox.
func (m *E2BMachine) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateRunning {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	body := map[string]interface{}{
		"templateID": m.template,
		"timeout":    m.timeout,
	}
	if len(m.envs) > 0 {
		body["envs"] = m.envs
	}
	if len(m.metadata) > 0 {
		body["metadata"] = m.metadata
	}

	respBody, err := m.apiRequest(ctx, http.MethodPost, "/sandboxes", body)
	if err != nil {
		return fmt.Errorf("start sandbox: %w", err)
	}

	var result struct {
		SandboxID string `json:"sandboxID"`
		ClientID  string `json:"clientID"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parse start response: %w", err)
	}

	m.mu.Lock()
	m.sandboxID = result.SandboxID
	m.clientID = result.ClientID
	if m.clientID == "" {
		m.clientID = result.SandboxID
	}
	m.state = StateRunning
	m.mu.Unlock()

	return nil
}

// Pause disconnects from the sandbox but keeps it alive until its timeout.
// E2B v1 has no native pause, so we simply release the connection.
func (m *E2BMachine) Pause(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state != StateRunning {
		return nil
	}

	m.state = StatePaused
	return nil
}

// Resume reconnects to a paused sandbox.
func (m *E2BMachine) Resume(ctx context.Context) error {
	m.mu.Lock()
	if m.state != StatePaused || m.sandboxID == "" {
		m.mu.Unlock()
		return nil
	}
	sandboxID := m.sandboxID
	m.mu.Unlock()

	// Verify the sandbox is still alive.
	_, err := m.GetInfo(ctx)
	if err != nil {
		return fmt.Errorf("resume sandbox %s: %w", sandboxID, err)
	}

	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	return nil
}

// Stop kills the sandbox and releases all resources.
func (m *E2BMachine) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateStopped {
		m.mu.Unlock()
		return nil
	}

	// If paused, we still have the sandbox ID to kill.
	sandboxID := m.sandboxID
	m.mu.Unlock()

	if sandboxID != "" {
		_, err := m.apiRequest(ctx, http.MethodDelete, "/sandboxes/"+sandboxID, nil)
		if err != nil {
			// Best effort — sandbox may have already timed out.
			_ = err
		}
	}

	m.mu.Lock()
	m.sandboxID = ""
	m.clientID = ""
	m.state = StateStopped
	m.mu.Unlock()

	return nil
}

// Execute runs a command inside the sandbox via the envd HTTP API.
func (m *E2BMachine) Execute(ctx context.Context, command string, opts *ExecuteOptions) (*ExecutionResult, error) {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine is %s — call Start() or Resume() first", m.state)
	}
	clientID := m.clientID
	m.mu.Unlock()

	reqBody := map[string]interface{}{
		"cmd": command,
	}
	if opts != nil {
		if opts.Cwd != "" {
			reqBody["workdir"] = opts.Cwd
		}
		if len(opts.Env) > 0 {
			reqBody["envs"] = opts.Env
		}
		if opts.Timeout > 0 {
			reqBody["timeout"] = opts.Timeout
		}
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal execute request: %w", err)
	}

	envdURL := fmt.Sprintf("https://%s-49982.e2b.dev/commands", clientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, envdURL, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create execute request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	execClient := &http.Client{}
	if opts != nil && opts.Timeout > 0 {
		execClient.Timeout = time.Duration(opts.Timeout+10) * time.Second
	}

	resp, err := execClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute command: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read execute response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("execute command failed (HTTP %d): %s", resp.StatusCode, string(respData))
	}

	var result struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, fmt.Errorf("parse execute response: %w", err)
	}

	// Call streaming callbacks with full output if provided.
	if opts != nil {
		if opts.OnStdout != nil && result.Stdout != "" {
			opts.OnStdout(result.Stdout)
		}
		if opts.OnStderr != nil && result.Stderr != "" {
			opts.OnStderr(result.Stderr)
		}
	}

	return &ExecutionResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

// ReadFile reads a file from the sandbox filesystem.
func (m *E2BMachine) ReadFile(ctx context.Context, path string) (string, error) {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return "", fmt.Errorf("machine is %s — call Start() or Resume() first", m.state)
	}
	clientID := m.clientID
	m.mu.Unlock()

	url := fmt.Sprintf("https://%s-49982.e2b.dev/files?path=%s", clientID, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create read request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read file response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read file failed (HTTP %d): %s", resp.StatusCode, string(data))
	}

	return string(data), nil
}

// WriteFile writes content to a file in the sandbox filesystem.
func (m *E2BMachine) WriteFile(ctx context.Context, path string, content []byte) error {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return fmt.Errorf("machine is %s — call Start() or Resume() first", m.state)
	}
	clientID := m.clientID
	m.mu.Unlock()

	url := fmt.Sprintf("https://%s-49982.e2b.dev/files?path=%s", clientID, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("create write request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("write file failed (HTTP %d): %s", resp.StatusCode, string(data))
	}

	return nil
}

// ListFiles lists file names in a directory in the sandbox filesystem.
func (m *E2BMachine) ListFiles(ctx context.Context, path string) ([]string, error) {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine is %s — call Start() or Resume() first", m.state)
	}
	clientID := m.clientID
	m.mu.Unlock()

	url := fmt.Sprintf("https://%s-49982.e2b.dev/files?path=%s&list=true", clientID, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read list response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list files failed (HTTP %d): %s", resp.StatusCode, string(data))
	}

	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse list response: %w", err)
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names, nil
}

// --- E2B-specific API ---

// GetSandboxID returns the current sandbox ID, or empty string if not started.
func (m *E2BMachine) GetSandboxID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sandboxID
}

// SetTimeout extends the sandbox timeout (in seconds).
func (m *E2BMachine) SetTimeout(ctx context.Context, timeout int) error {
	m.mu.Lock()
	sandboxID := m.sandboxID
	m.mu.Unlock()

	if sandboxID == "" {
		return fmt.Errorf("machine not started")
	}

	body := map[string]interface{}{
		"timeout": timeout,
	}

	_, err := m.apiRequest(ctx, http.MethodPatch, "/sandboxes/"+sandboxID, body)
	return err
}

// GetInfo returns sandbox metadata and status information.
func (m *E2BMachine) GetInfo(ctx context.Context) (map[string]interface{}, error) {
	m.mu.Lock()
	sandboxID := m.sandboxID
	m.mu.Unlock()

	if sandboxID == "" {
		return nil, fmt.Errorf("machine not started")
	}

	respBody, err := m.apiRequest(ctx, http.MethodGet, "/sandboxes/"+sandboxID, nil)
	if err != nil {
		return nil, err
	}

	var info map[string]interface{}
	if err := json.Unmarshal(respBody, &info); err != nil {
		return nil, fmt.Errorf("parse info response: %w", err)
	}
	return info, nil
}

// MakeDir creates a directory inside the sandbox.
func (m *E2BMachine) MakeDir(ctx context.Context, path string) error {
	// Use execute to mkdir since the files API may not support mkdir directly.
	_, err := m.Execute(ctx, "mkdir -p "+path, nil)
	return err
}

// Remove removes a file or directory inside the sandbox.
func (m *E2BMachine) Remove(ctx context.Context, path string) error {
	_, err := m.Execute(ctx, "rm -rf "+path, nil)
	return err
}

// Exists checks whether a path exists inside the sandbox.
func (m *E2BMachine) Exists(ctx context.Context, path string) (bool, error) {
	result, err := m.Execute(ctx, fmt.Sprintf("test -e %s && echo yes || echo no", path), nil)
	if err != nil {
		return false, err
	}
	return result.Stdout == "yes\n" || result.Stdout == "yes", nil
}

// IsRunning checks if the sandbox is still alive.
func (m *E2BMachine) IsRunning(ctx context.Context) bool {
	m.mu.Lock()
	sandboxID := m.sandboxID
	m.mu.Unlock()

	if sandboxID == "" {
		return false
	}

	_, err := m.GetInfo(ctx)
	return err == nil
}

// GetHost returns the hostname for accessing a port on the sandbox.
func (m *E2BMachine) GetHost(port int) string {
	m.mu.Lock()
	clientID := m.clientID
	m.mu.Unlock()

	return fmt.Sprintf("%s-%d.e2b.dev", clientID, port)
}

// apiRequest makes an authenticated request to the E2B REST API.
func (m *E2BMachine) apiRequest(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(jsonData)
	}

	url := e2bAPIBase + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API %s %s returned HTTP %d: %s", method, path, resp.StatusCode, string(respData))
	}

	return respData, nil
}

// Compile-time check that E2BMachine implements Machine.
var _ Machine = (*E2BMachine)(nil)
