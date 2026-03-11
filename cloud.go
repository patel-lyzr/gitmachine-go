package gitmachine

import "context"

// CloudInstance holds the details of a running cloud VM instance.
type CloudInstance struct {
	// ID is the provider-specific instance identifier (e.g. EC2 instance ID, Azure VM ID, GCP instance name).
	ID string

	// PublicIP is the public IP address for SSH access.
	PublicIP string
}

// CloudProvider abstracts cloud VM lifecycle operations across AWS, Azure, and GCP.
// Implementations handle only cloud API calls — SSH and command execution are handled
// by CloudMachine which wraps any CloudProvider.
type CloudProvider interface {
	// Name returns the provider name (e.g. "aws", "azure", "gcp").
	Name() string

	// Launch creates a new VM instance and returns its details.
	// The instance must be fully running and have a public IP when this returns.
	Launch(ctx context.Context) (*CloudInstance, error)

	// StopInstance stops (but does not terminate) the instance, preserving its disk.
	StopInstance(ctx context.Context, id string) error

	// StartInstance starts a previously stopped instance.
	// Must update and return the new CloudInstance since the IP may change.
	StartInstance(ctx context.Context, id string) (*CloudInstance, error)

	// Terminate permanently destroys the instance and cleans up associated resources
	// (temporary key pairs, security groups, firewall rules, etc.).
	Terminate(ctx context.Context, id string) error

	// Describe returns the current details of an instance, or error if not found.
	Describe(ctx context.Context, id string) (*CloudInstance, error)

	// SSHConfig returns the SSH user and private key PEM for connecting to instances.
	SSHConfig() (user string, privateKeyPEM string)
}
