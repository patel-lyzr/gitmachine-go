package gitmachine

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	cloudSSHRetryDelay = 5 * time.Second
	cloudSSHMaxRetries = 24
)

// CloudMachine implements Machine using any CloudProvider for VM lifecycle
// and SSH for command execution. This avoids duplicating SSH logic across
// AWS, Azure, and GCP implementations.
type CloudMachine struct {
	mu    sync.Mutex
	state MachineState

	provider   CloudProvider
	instanceID string
	publicIP   string
	sshClient  *ssh.Client
	sshCfg     *ssh.ClientConfig
}

// NewCloudMachine creates a Machine backed by the given CloudProvider.
func NewCloudMachine(provider CloudProvider) *CloudMachine {
	return &CloudMachine{
		state:    StateIdle,
		provider: provider,
	}
}

// ConnectCloudMachine connects to an existing instance by ID.
func ConnectCloudMachine(ctx context.Context, provider CloudProvider, instanceID string) (*CloudMachine, error) {
	m := NewCloudMachine(provider)

	inst, err := provider.Describe(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("describe instance %s: %w", instanceID, err)
	}
	if inst.PublicIP == "" {
		return nil, fmt.Errorf("instance %s has no public IP", instanceID)
	}

	m.instanceID = inst.ID
	m.publicIP = inst.PublicIP

	if err := m.buildSSHConfig(); err != nil {
		return nil, err
	}
	if err := m.connectSSH(); err != nil {
		return nil, fmt.Errorf("ssh connect: %w", err)
	}

	m.state = StateRunning
	return m, nil
}

func (m *CloudMachine) ID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instanceID
}

func (m *CloudMachine) State() MachineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *CloudMachine) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateRunning {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	inst, err := m.provider.Launch(ctx)
	if err != nil {
		return fmt.Errorf("%s launch: %w", m.provider.Name(), err)
	}

	m.mu.Lock()
	m.instanceID = inst.ID
	m.publicIP = inst.PublicIP
	m.mu.Unlock()

	if err := m.buildSSHConfig(); err != nil {
		return err
	}
	if err := m.waitForSSH(); err != nil {
		return fmt.Errorf("wait for ssh: %w", err)
	}

	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	return nil
}

func (m *CloudMachine) Pause(ctx context.Context) error {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.instanceID
	m.mu.Unlock()

	m.closeSSH()

	if err := m.provider.StopInstance(ctx, instanceID); err != nil {
		return fmt.Errorf("%s stop: %w", m.provider.Name(), err)
	}

	m.mu.Lock()
	m.state = StatePaused
	m.mu.Unlock()
	return nil
}

func (m *CloudMachine) Resume(ctx context.Context) error {
	m.mu.Lock()
	if m.state != StatePaused || m.instanceID == "" {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.instanceID
	m.mu.Unlock()

	inst, err := m.provider.StartInstance(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("%s start: %w", m.provider.Name(), err)
	}

	m.mu.Lock()
	m.publicIP = inst.PublicIP
	m.mu.Unlock()

	if err := m.waitForSSH(); err != nil {
		return fmt.Errorf("wait for ssh: %w", err)
	}

	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	return nil
}

func (m *CloudMachine) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateStopped {
		m.mu.Unlock()
		return nil
	}
	instanceID := m.instanceID
	m.mu.Unlock()

	m.closeSSH()

	if instanceID != "" {
		_ = m.provider.Terminate(ctx, instanceID)
	}

	m.mu.Lock()
	m.instanceID = ""
	m.publicIP = ""
	m.state = StateStopped
	m.mu.Unlock()
	return nil
}

func (m *CloudMachine) Execute(ctx context.Context, command string, opts *ExecuteOptions) (*ExecutionResult, error) {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine is %s — call Start() or Resume() first", m.state)
	}
	client := m.sshClient
	m.mu.Unlock()

	if client == nil {
		return nil, fmt.Errorf("ssh not connected")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("create ssh session: %w", err)
	}
	defer session.Close()

	fullCmd := command
	if opts != nil {
		if len(opts.Env) > 0 {
			var envPrefix strings.Builder
			for k, v := range opts.Env {
				envPrefix.WriteString(fmt.Sprintf("export %s=%q; ", k, v))
			}
			fullCmd = envPrefix.String() + fullCmd
		}
		if opts.Cwd != "" {
			fullCmd = fmt.Sprintf("cd %s && %s", opts.Cwd, fullCmd)
		}
	}

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if opts != nil && opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(opts.Timeout)*time.Second)
		defer cancel()
	}

	done := make(chan error, 1)
	go func() {
		done <- session.Run(fullCmd)
	}()

	var exitCode int
	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return nil, fmt.Errorf("command timed out: %w", ctx.Err())
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				exitCode = exitErr.ExitStatus()
			} else {
				return nil, fmt.Errorf("execute command: %w", err)
			}
		}
	}

	stdoutStr := stdout.String()
	stderrStr := stderr.String()

	if opts != nil {
		if opts.OnStdout != nil && stdoutStr != "" {
			opts.OnStdout(stdoutStr)
		}
		if opts.OnStderr != nil && stderrStr != "" {
			opts.OnStderr(stderrStr)
		}
	}

	return &ExecutionResult{
		ExitCode: exitCode,
		Stdout:   stdoutStr,
		Stderr:   stderrStr,
	}, nil
}

func (m *CloudMachine) ReadFile(ctx context.Context, path string) (string, error) {
	result, err := m.Execute(ctx, fmt.Sprintf("cat %s", path), nil)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read file %s: %s", path, result.Stderr)
	}
	return result.Stdout, nil
}

func (m *CloudMachine) WriteFile(ctx context.Context, path string, content []byte) error {
	m.mu.Lock()
	if m.state != StateRunning || m.sshClient == nil {
		m.mu.Unlock()
		return fmt.Errorf("machine is %s — call Start() or Resume() first", m.state)
	}
	client := m.sshClient
	m.mu.Unlock()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("create ssh session: %w", err)
	}
	defer session.Close()

	session.Stdin = bytes.NewReader(content)
	if err := session.Run(fmt.Sprintf("cat > %s", path)); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

func (m *CloudMachine) ListFiles(ctx context.Context, path string) ([]string, error) {
	result, err := m.Execute(ctx, fmt.Sprintf("ls -1 %s", path), nil)
	if err != nil {
		return nil, fmt.Errorf("list files %s: %w", path, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("list files %s: %s", path, result.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	return lines, nil
}

// --- SSH internals ---

func (m *CloudMachine) buildSSHConfig() error {
	_, privateKeyPEM := m.provider.SSHConfig()
	signer, err := ssh.ParsePrivateKey([]byte(privateKeyPEM))
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}

	user, _ := m.provider.SSHConfig()
	m.sshCfg = &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         10 * time.Second,
	}
	return nil
}

func (m *CloudMachine) connectSSH() error {
	addr := net.JoinHostPort(m.publicIP, "22")
	client, err := ssh.Dial("tcp", addr, m.sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	m.mu.Lock()
	m.sshClient = client
	m.mu.Unlock()
	return nil
}

func (m *CloudMachine) closeSSH() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sshClient != nil {
		_ = m.sshClient.Close()
		m.sshClient = nil
	}
}

func (m *CloudMachine) waitForSSH() error {
	for i := 0; i < cloudSSHMaxRetries; i++ {
		if err := m.connectSSH(); err == nil {
			return nil
		}
		time.Sleep(cloudSSHRetryDelay)
	}
	return fmt.Errorf("ssh not available after %d retries on %s", cloudSSHMaxRetries, m.publicIP)
}

// GetPublicIP returns the instance's current public IP.
func (m *CloudMachine) GetPublicIP() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publicIP
}

// Provider returns the underlying CloudProvider.
func (m *CloudMachine) Provider() CloudProvider {
	return m.provider
}

var _ Machine = (*CloudMachine)(nil)
