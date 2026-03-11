package gitmachine

import "time"

// MachineState represents the current lifecycle state of a Machine.
type MachineState string

const (
	StateIdle    MachineState = "idle"
	StateRunning MachineState = "running"
	StatePaused  MachineState = "paused"
	StateStopped MachineState = "stopped"
)

// ExecutionResult holds the output of a command execution.
type ExecutionResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// LogEntry records a single command execution with its result and timestamp.
type LogEntry struct {
	Command   string          `json:"command"`
	Result    ExecutionResult `json:"result"`
	Timestamp time.Time       `json:"timestamp"`
}

// OnOutput is a callback invoked with stdout or stderr data as it streams.
type OnOutput func(data string)

// OnExit is a callback invoked when a command exits.
type OnExit func(exitCode int)

// OnEvent is a callback invoked on lifecycle events from GitMachine.
type OnEvent func(event string, data map[string]interface{})

// RunOptions configures a GitMachine.Run() call.
type RunOptions struct {
	// Cwd overrides the working directory for the command.
	Cwd string

	// Env provides additional environment variables for the command.
	Env map[string]string

	// Timeout in seconds. Zero means no timeout.
	Timeout int

	// OnStdout is called with stdout data as it streams.
	OnStdout OnOutput

	// OnStderr is called with stderr data as it streams.
	OnStderr OnOutput

	// OnExit is called when the command exits.
	OnExit OnExit
}

// ExecuteOptions configures a Machine.Execute() call.
type ExecuteOptions struct {
	// Cwd overrides the working directory for the command.
	Cwd string

	// Env provides additional environment variables for the command.
	Env map[string]string

	// Timeout in seconds. Zero means no timeout.
	Timeout int

	// OnStdout is called with stdout data as it streams.
	OnStdout OnOutput

	// OnStderr is called with stderr data as it streams.
	OnStderr OnOutput
}

// GitIdentity configures the git user identity for commits.
type GitIdentity struct {
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatarUrl,omitempty"`
}

// LifecycleHook is a callback invoked at lifecycle transitions of a GitMachine.
type LifecycleHook func(gm *GitMachine) error

// GitMachineConfig holds all configuration for constructing a GitMachine.
type GitMachineConfig struct {
	// Machine is the underlying VM provider (required).
	Machine Machine

	// Repository is the git repo URL, e.g. "https://github.com/user/repo.git" (required).
	Repository string

	// Token is a personal access token for git authentication (required).
	Token string

	// Identity configures the git user for commits. Defaults to GitMachine/gitagent@machine.
	Identity *GitIdentity

	// OnStart is called after the repo is cloned and ready.
	OnStart LifecycleHook

	// OnPause is called before the machine pauses.
	OnPause LifecycleHook

	// OnResume is called after the machine resumes.
	OnResume LifecycleHook

	// OnEnd is called before the machine stops.
	OnEnd LifecycleHook

	// Env provides environment variables for all commands.
	Env map[string]string

	// Timeout in seconds for command execution. Zero means no timeout.
	Timeout int

	// Session is the branch name for session persistence. Empty means use default branch.
	Session string

	// AutoCommit enables automatic commit on pause/stop. Defaults to true.
	// Use a pointer to distinguish between "not set" (nil = true) and "explicitly false".
	AutoCommit *bool

	// OnEvent is called on lifecycle events (started, paused, resumed, etc.).
	OnEvent func(event string, data map[string]interface{}, gm *GitMachine)
}

// E2BMachineConfig holds configuration for constructing an E2BMachine.
type E2BMachineConfig struct {
	// APIKey is the E2B API key. Falls back to E2B_API_KEY env var.
	APIKey string

	// Template is the sandbox template name. Defaults to "base".
	Template string

	// Timeout in seconds for the sandbox lifetime. Defaults to 300.
	Timeout int

	// Envs provides environment variables inside the sandbox.
	Envs map[string]string

	// Metadata attaches key-value metadata to the sandbox.
	Metadata map[string]string
}

// EC2MachineConfig holds configuration for constructing an EC2Machine.
type EC2MachineConfig struct {
	// AccessKeyID is the AWS access key. Falls back to AWS_ACCESS_KEY_ID env var.
	AccessKeyID string

	// SecretAccessKey is the AWS secret key. Falls back to AWS_SECRET_ACCESS_KEY env var.
	SecretAccessKey string

	// Region is the AWS region (e.g. "us-east-1"). Falls back to AWS_REGION env var. Defaults to "us-east-1".
	Region string

	// AMI is the Amazon Machine Image ID. Defaults to latest Ubuntu 22.04 LTS.
	AMI string

	// InstanceType is the EC2 instance type. Defaults to "t3.medium".
	InstanceType string

	// KeyName is the name of an existing EC2 key pair for SSH access.
	// If empty, a temporary key pair is created and cleaned up on Stop.
	KeyName string

	// PrivateKeyPEM is the PEM-encoded private key for SSH.
	// Required if KeyName is provided. Auto-generated if KeyName is empty.
	PrivateKeyPEM string

	// SecurityGroupIDs specifies security groups. Must allow inbound SSH (port 22).
	// If empty, a temporary security group is created and cleaned up on Stop.
	SecurityGroupIDs []string

	// SubnetID optionally places the instance in a specific subnet.
	SubnetID string

	// SSHUser is the SSH username. Defaults to "ubuntu".
	SSHUser string

	// Tags are applied to all created AWS resources.
	Tags map[string]string
}

// PortMapping represents a container-to-host port mapping.
type PortMapping struct {
	// ContainerPort is the port inside the container.
	ContainerPort int `json:"container_port"`

	// HostPort is the mapped port on the host node. Auto-assigned if zero.
	HostPort int `json:"host_port"`

	// URL is the publicly accessible URL (computed: http://<node-ip>:<host-port>).
	URL string `json:"url,omitempty"`
}

// DockerMachineConfig holds configuration for constructing a DockerMachine.
type DockerMachineConfig struct {
	// Image is the Docker image to use. Defaults to "ubuntu:22.04".
	Image string

	// Name is the container name. Auto-generated if empty.
	Name string

	// CPUs is the number of CPUs to allocate (e.g. "1", "0.5", "2"). Maps to --cpus.
	CPUs string

	// Memory is the memory limit (e.g. "512m", "2g"). Maps to --memory.
	Memory string

	// DiskSize is the writable layer storage limit (e.g. "10g", "20g"). Maps to --storage-opt size=.
	// Requires the overlay2 storage driver with xfs and pquota mount option on the Docker host.
	DiskSize string

	// Ports lists container ports to expose. Host ports are auto-assigned if zero.
	Ports []int
}
