package gitmachine

import "context"

// Machine is the interface that all VM providers must implement.
// It abstracts sandbox lifecycle and command execution.
type Machine interface {
	// ID returns the unique identifier of the machine, or empty string if not started.
	ID() string

	// State returns the current lifecycle state.
	State() MachineState

	// Start initializes and starts the machine.
	Start(ctx context.Context) error

	// Pause suspends the machine, keeping its state for later resumption.
	Pause(ctx context.Context) error

	// Resume restores a paused machine to running state.
	Resume(ctx context.Context) error

	// Stop terminates the machine and releases all resources.
	Stop(ctx context.Context) error

	// Execute runs a command inside the machine and returns the result.
	Execute(ctx context.Context, command string, opts *ExecuteOptions) (*ExecutionResult, error)

	// ReadFile reads the contents of a file inside the machine.
	ReadFile(ctx context.Context, path string) (string, error)

	// WriteFile writes content to a file inside the machine.
	WriteFile(ctx context.Context, path string, content []byte) error

	// ListFiles lists file names in a directory inside the machine.
	ListFiles(ctx context.Context, path string) ([]string, error)
}
