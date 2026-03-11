package gitmachine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// DockerMachine implements Machine by running Docker containers on a CloudMachine node.
// Commands are executed via `docker exec` over SSH.
type DockerMachine struct {
	mu    sync.Mutex
	state MachineState

	containerID string
	name        string
	image       string
	cpus        string
	memory      string
	diskSize    string
	nodeID      string
	node        *CloudMachine

	dockerReady bool
	useSudo     bool // prefix docker commands with sudo when user lacks group access
}

// NewDockerMachine creates a DockerMachine backed by the given CloudMachine node.
func NewDockerMachine(node *CloudMachine, config DockerMachineConfig) *DockerMachine {
	image := config.Image
	if image == "" {
		image = "ubuntu:22.04"
	}

	name := config.Name
	if name == "" {
		name = generateSandboxName()
	}

	return &DockerMachine{
		state:    StateIdle,
		name:     name,
		image:    image,
		cpus:     config.CPUs,
		memory:   config.Memory,
		diskSize: config.DiskSize,
		nodeID:   node.ID(),
		node:     node,
	}
}

// ConnectDockerMachine reconnects to an existing container on a node.
func ConnectDockerMachine(node *CloudMachine, containerID string) (*DockerMachine, error) {
	// Detect if sudo is needed.
	useSudo := false
	check, err := node.Execute(context.Background(), "docker version", nil)
	if err != nil || check.ExitCode != 0 {
		useSudo = true
	}

	// Verify container exists.
	inspectCmd := fmt.Sprintf("docker inspect --format '{{.State.Status}}' %s", containerID)
	if useSudo {
		inspectCmd = "sudo " + inspectCmd
	}
	result, err := node.Execute(context.Background(), inspectCmd, nil)
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", containerID, err)
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("container %s not found: %s", containerID, result.Stderr)
	}

	status := strings.TrimSpace(result.Stdout)

	dm := &DockerMachine{
		containerID: containerID,
		nodeID:      node.ID(),
		node:        node,
		dockerReady: true,
		useSudo:     useSudo,
	}

	switch status {
	case "running":
		dm.state = StateRunning
	case "paused":
		dm.state = StatePaused
	default:
		dm.state = StateStopped
	}

	return dm, nil
}

func (m *DockerMachine) ID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.containerID
}

func (m *DockerMachine) State() MachineState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *DockerMachine) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateRunning {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	if err := m.ensureDocker(ctx); err != nil {
		return err
	}

	// Pull image first (docker run will also pull, but this gives clearer errors).
	pullResult, err := m.node.Execute(ctx, m.dockerCmd(fmt.Sprintf("docker pull %s", m.image)), nil)
	if err != nil {
		return fmt.Errorf("pull image %s: %w", m.image, err)
	}
	if pullResult.ExitCode != 0 {
		return fmt.Errorf("pull image %s: %s", m.image, pullResult.Stderr)
	}

	// Run container in detached mode with sleep infinity to keep it alive.
	runArgs := fmt.Sprintf("docker run -d --name %s --hostname %s", m.name, m.name)
	if m.cpus != "" {
		runArgs += fmt.Sprintf(" --cpus %s", m.cpus)
	}
	if m.memory != "" {
		runArgs += fmt.Sprintf(" --memory %s", m.memory)
	}
	if m.diskSize != "" {
		runArgs += fmt.Sprintf(" --storage-opt size=%s", m.diskSize)
	}
	runArgs += fmt.Sprintf(" %s sleep infinity", m.image)
	cmd := m.dockerCmd(runArgs)
	result, err := m.node.Execute(ctx, cmd, nil)
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("start container: %s", result.Stderr)
	}

	containerID := strings.TrimSpace(result.Stdout)

	m.mu.Lock()
	m.containerID = containerID
	m.state = StateRunning
	m.mu.Unlock()

	return nil
}

func (m *DockerMachine) Pause(ctx context.Context) error {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return nil
	}
	cid := m.containerID
	m.mu.Unlock()

	result, err := m.node.Execute(ctx, m.dockerCmd(fmt.Sprintf("docker pause %s", cid)), nil)
	if err != nil {
		return fmt.Errorf("pause container: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("pause container: %s", result.Stderr)
	}

	m.mu.Lock()
	m.state = StatePaused
	m.mu.Unlock()
	return nil
}

func (m *DockerMachine) Resume(ctx context.Context) error {
	m.mu.Lock()
	if m.state != StatePaused {
		m.mu.Unlock()
		return nil
	}
	cid := m.containerID
	m.mu.Unlock()

	result, err := m.node.Execute(ctx, m.dockerCmd(fmt.Sprintf("docker unpause %s", cid)), nil)
	if err != nil {
		return fmt.Errorf("resume container: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("resume container: %s", result.Stderr)
	}

	m.mu.Lock()
	m.state = StateRunning
	m.mu.Unlock()
	return nil
}

func (m *DockerMachine) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateStopped {
		m.mu.Unlock()
		return nil
	}
	cid := m.containerID
	m.mu.Unlock()

	if cid != "" {
		_, _ = m.node.Execute(ctx, m.dockerCmd(fmt.Sprintf("docker rm -f %s", cid)), nil)
	}

	m.mu.Lock()
	m.containerID = ""
	m.state = StateStopped
	m.mu.Unlock()
	return nil
}

func (m *DockerMachine) Execute(ctx context.Context, command string, opts *ExecuteOptions) (*ExecutionResult, error) {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox is %s — call Start() or Resume() first", m.state)
	}
	cid := m.containerID
	m.mu.Unlock()

	// Build docker exec command with env vars and working directory.
	var parts []string
	if m.useSudo {
		parts = append(parts, "sudo")
	}
	parts = append(parts, "docker", "exec")

	if opts != nil {
		for k, v := range opts.Env {
			parts = append(parts, "-e", fmt.Sprintf("%s=%s", k, v))
		}
		if opts.Cwd != "" {
			parts = append(parts, "-w", opts.Cwd)
		}
	}

	parts = append(parts, cid, "sh", "-c", shellQuote(command))

	fullCmd := strings.Join(parts, " ")

	// Pass timeout through to node execution but clear env/cwd since we handled them.
	nodeOpts := &ExecuteOptions{}
	if opts != nil {
		nodeOpts.Timeout = opts.Timeout
		nodeOpts.OnStdout = opts.OnStdout
		nodeOpts.OnStderr = opts.OnStderr
	}

	return m.node.Execute(ctx, fullCmd, nodeOpts)
}

func (m *DockerMachine) ReadFile(ctx context.Context, path string) (string, error) {
	result, err := m.Execute(ctx, fmt.Sprintf("cat %s", path), nil)
	if err != nil {
		return "", fmt.Errorf("read file %s: %w", path, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("read file %s: %s", path, result.Stderr)
	}
	return result.Stdout, nil
}

func (m *DockerMachine) WriteFile(ctx context.Context, path string, content []byte) error {
	m.mu.Lock()
	if m.state != StateRunning {
		m.mu.Unlock()
		return fmt.Errorf("sandbox is %s — call Start() or Resume() first", m.state)
	}
	cid := m.containerID
	m.mu.Unlock()

	cmd := m.dockerCmd(fmt.Sprintf("docker exec -i %s sh -c 'cat > %s'", cid, path))
	result, err := m.node.RunWithStdin(ctx, cmd, bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write file %s: %s", path, result.Stderr)
	}
	return nil
}

func (m *DockerMachine) ListFiles(ctx context.Context, path string) ([]string, error) {
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

// GetName returns the container name.
func (m *DockerMachine) GetName() string {
	return m.name
}

// --- internal helpers ---

func (m *DockerMachine) ensureDocker(ctx context.Context) error {
	if m.dockerReady {
		return nil
	}

	// Check if Docker is already accessible without sudo.
	result, err := m.node.Execute(ctx, "docker version", nil)
	if err == nil && result.ExitCode == 0 {
		m.dockerReady = true
		return nil
	}

	// Check if Docker is installed but needs sudo.
	result, err = m.node.Execute(ctx, "sudo docker version", nil)
	if err == nil && result.ExitCode == 0 {
		m.useSudo = true
		m.dockerReady = true
		return nil
	}

	// Install Docker.
	fmt.Println("Installing Docker on node...")
	installCmd := "curl -fsSL https://get.docker.com | sudo sh && sudo usermod -aG docker $USER"
	result, err = m.node.Execute(ctx, installCmd, &ExecuteOptions{Timeout: 300})
	if err != nil {
		return fmt.Errorf("install docker: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("install docker failed: %s", result.Stderr)
	}

	// After fresh install, the current session won't have the docker group.
	// Use sudo for this session.
	m.useSudo = true
	m.dockerReady = true
	return nil
}

// dockerCmd prefixes a docker command with sudo if needed.
func (m *DockerMachine) dockerCmd(cmd string) string {
	if m.useSudo {
		return "sudo " + cmd
	}
	return cmd
}

func generateSandboxName() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "gm-" + hex.EncodeToString(b)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

var _ Machine = (*DockerMachine)(nil)
