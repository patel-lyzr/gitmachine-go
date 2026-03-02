package gitmachine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const repoPath = "/home/user/repo"

// GitMachine wraps a Machine with git lifecycle management.
// It clones a repository on start, auto-commits on pause/stop,
// and persists sessions as git branches.
type GitMachine struct {
	mu sync.Mutex

	machine    Machine
	repository string
	token      string
	identity   GitIdentity
	onStartCb  LifecycleHook
	onPauseCb  LifecycleHook
	onResumeCb LifecycleHook
	onEndCb    LifecycleHook
	env        map[string]string
	session    string
	autoCommit bool
	onEventCb  func(event string, data map[string]interface{}, gm *GitMachine)

	logs           []LogEntry
	skipAutoCommit bool
}

// NewGitMachine creates a new GitMachine with the given configuration.
func NewGitMachine(config GitMachineConfig) *GitMachine {
	identity := GitIdentity{Name: "GitMachine", Email: "gitagent@machine"}
	if config.Identity != nil {
		identity = *config.Identity
	}

	autoCommit := true
	if config.AutoCommit != nil {
		autoCommit = *config.AutoCommit
	}

	env := make(map[string]string)
	for k, v := range config.Env {
		env[k] = v
	}

	return &GitMachine{
		machine:    config.Machine,
		repository: config.Repository,
		token:      config.Token,
		identity:   identity,
		onStartCb:  config.OnStart,
		onPauseCb:  config.OnPause,
		onResumeCb: config.OnResume,
		onEndCb:    config.OnEnd,
		env:        env,
		session:    config.Session,
		autoCommit: autoCommit,
		onEventCb:  config.OnEvent,
	}
}

// ConnectGitMachine reconnects to an already-running GitMachine by its machine.
// The sandbox must still be alive and the repo already cloned.
func ConnectGitMachine(machine Machine, config GitMachineConfig) (*GitMachine, error) {
	config.Machine = machine
	gm := NewGitMachine(config)
	gm.emit("reconnected", map[string]interface{}{"id": machine.ID()})
	return gm, nil
}

// ID returns the underlying machine's ID.
func (gm *GitMachine) ID() string {
	return gm.machine.ID()
}

// State returns the underlying machine's state.
func (gm *GitMachine) State() MachineState {
	return gm.machine.State()
}

// Path returns the repository path inside the VM.
func (gm *GitMachine) Path() string {
	return repoPath
}

// --- Lifecycle ---

// Start initializes the machine, clones the repository, checks out the session
// branch (if configured), configures git identity, and invokes the OnStart hook.
func (gm *GitMachine) Start(ctx context.Context) error {
	if err := gm.machine.Start(ctx); err != nil {
		return fmt.Errorf("start machine: %w", err)
	}

	// Clone with token embedded in URL (stays inside sandbox).
	authURL := gm.authURL()
	if _, err := gm.machine.Execute(ctx, fmt.Sprintf("git clone %s %s", authURL, repoPath), nil); err != nil {
		return fmt.Errorf("clone repo: %w", err)
	}

	// Checkout session branch if specified.
	if gm.session != "" {
		cmd := fmt.Sprintf("git checkout %s 2>/dev/null || git checkout -b %s", gm.session, gm.session)
		if _, err := gm.exec(ctx, cmd); err != nil {
			return fmt.Errorf("checkout session branch: %w", err)
		}
	}

	// Configure git identity for commits.
	if _, err := gm.exec(ctx, fmt.Sprintf(`git config user.name "%s"`, gm.identity.Name)); err != nil {
		return fmt.Errorf("set git user.name: %w", err)
	}
	if _, err := gm.exec(ctx, fmt.Sprintf(`git config user.email "%s"`, gm.identity.Email)); err != nil {
		return fmt.Errorf("set git user.email: %w", err)
	}

	gm.emit("started", map[string]interface{}{
		"session":  gm.session,
		"repoPath": repoPath,
	})

	if gm.onStartCb != nil {
		if err := gm.onStartCb(gm); err != nil {
			return fmt.Errorf("onStart hook: %w", err)
		}
	}

	return nil
}

// Pause auto-commits (if enabled), invokes the OnPause hook, and pauses the machine.
func (gm *GitMachine) Pause(ctx context.Context) error {
	gm.mu.Lock()
	skip := gm.skipAutoCommit
	gm.mu.Unlock()

	if gm.autoCommit && !skip {
		gm.autoCommitChanges(ctx)
	}

	if gm.onPauseCb != nil {
		if err := gm.onPauseCb(gm); err != nil {
			return fmt.Errorf("onPause hook: %w", err)
		}
	}

	if err := gm.machine.Pause(ctx); err != nil {
		return fmt.Errorf("pause machine: %w", err)
	}

	gm.emit("paused", map[string]interface{}{})
	return nil
}

// Resume restores a paused machine and invokes the OnResume hook.
func (gm *GitMachine) Resume(ctx context.Context) error {
	if err := gm.machine.Resume(ctx); err != nil {
		return fmt.Errorf("resume machine: %w", err)
	}

	if gm.onResumeCb != nil {
		if err := gm.onResumeCb(gm); err != nil {
			return fmt.Errorf("onResume hook: %w", err)
		}
	}

	gm.emit("resumed", map[string]interface{}{})
	return nil
}

// Stop auto-commits, pushes, invokes the OnEnd hook, and stops the machine.
func (gm *GitMachine) Stop(ctx context.Context) error {
	if gm.autoCommit {
		gm.autoCommitChanges(ctx)
		gm.pushChanges(ctx)
	}

	gm.emit("stopping", map[string]interface{}{})

	if gm.onEndCb != nil {
		if err := gm.onEndCb(gm); err != nil {
			return fmt.Errorf("onEnd hook: %w", err)
		}
	}

	if err := gm.machine.Stop(ctx); err != nil {
		return fmt.Errorf("stop machine: %w", err)
	}

	gm.emit("stopped", map[string]interface{}{})
	return nil
}

// --- Git Operations ---

// Diff returns the git diff of all changes against HEAD.
// Uses the whileRunning pattern: if paused, transparently resumes and re-pauses.
func (gm *GitMachine) Diff(ctx context.Context) (string, error) {
	var diff string
	err := gm.whileRunning(ctx, func() error {
		// Stage everything first so untracked files show in the diff.
		if _, err := gm.exec(ctx, "git add -A"); err != nil {
			return err
		}
		result, err := gm.exec(ctx, "git diff --cached")
		if err != nil {
			return err
		}
		// Unstage so we don't affect working state.
		if _, err := gm.exec(ctx, "git reset HEAD --quiet"); err != nil {
			return err
		}
		diff = result.Stdout
		return nil
	})
	return diff, err
}

// Commit stages all changes and commits them. Returns the commit SHA, or empty
// string if there was nothing to commit. Uses the whileRunning pattern.
func (gm *GitMachine) Commit(ctx context.Context, message string) (string, error) {
	var sha string
	err := gm.whileRunning(ctx, func() error {
		if _, err := gm.exec(ctx, "git add -A"); err != nil {
			return err
		}

		// Check if there are staged changes.
		check, err := gm.exec(ctx, "git diff --cached --quiet")
		if err != nil {
			return err
		}
		if check.ExitCode == 0 {
			// Nothing to commit.
			return nil
		}

		if message == "" {
			message = "checkpoint"
		}
		if _, err := gm.exec(ctx, fmt.Sprintf(`git commit -m "%s"`, message)); err != nil {
			return err
		}

		result, err := gm.exec(ctx, "git rev-parse HEAD")
		if err != nil {
			return err
		}
		sha = strings.TrimSpace(result.Stdout)

		gm.emit("committed", map[string]interface{}{
			"sha":     sha,
			"message": message,
		})
		return nil
	})
	return sha, err
}

// Push pushes changes to the remote origin. Uses the whileRunning pattern.
func (gm *GitMachine) Push(ctx context.Context) error {
	return gm.whileRunning(ctx, func() error {
		gm.pushChanges(ctx)
		gm.emit("pushed", map[string]interface{}{})
		return nil
	})
}

// Pull pulls changes from the remote origin. Uses the whileRunning pattern.
func (gm *GitMachine) Pull(ctx context.Context) error {
	return gm.whileRunning(ctx, func() error {
		branch := gm.session
		if branch == "" {
			branch = "main"
		}
		if _, err := gm.exec(ctx, fmt.Sprintf("git pull origin %s", branch)); err != nil {
			return err
		}
		gm.emit("pulled", map[string]interface{}{})
		return nil
	})
}

// Hash returns the current HEAD commit SHA. Uses the whileRunning pattern.
func (gm *GitMachine) Hash(ctx context.Context) (string, error) {
	var hash string
	err := gm.whileRunning(ctx, func() error {
		result, err := gm.exec(ctx, "git rev-parse HEAD")
		if err != nil {
			return err
		}
		hash = strings.TrimSpace(result.Stdout)
		return nil
	})
	return hash, err
}

// --- Runtime ---

// Update merges new environment variables and/or calls an update callback.
func (gm *GitMachine) Update(env map[string]string, onUpdate func(gm *GitMachine) error) error {
	gm.mu.Lock()
	for k, v := range env {
		gm.env[k] = v
	}
	gm.mu.Unlock()

	if onUpdate != nil {
		return onUpdate(gm)
	}
	return nil
}

// Run executes a command in the sandbox with the configured environment.
// The command runs in the repo directory by default.
func (gm *GitMachine) Run(ctx context.Context, command string, opts *RunOptions) (*ExecutionResult, error) {
	gm.mu.Lock()
	mergedEnv := make(map[string]string)
	for k, v := range gm.env {
		mergedEnv[k] = v
	}
	gm.mu.Unlock()

	cwd := repoPath
	var timeout int
	var onStdout, onStderr OnOutput
	var onExit OnExit

	if opts != nil {
		if opts.Cwd != "" {
			cwd = opts.Cwd
		}
		for k, v := range opts.Env {
			mergedEnv[k] = v
		}
		timeout = opts.Timeout
		onStdout = opts.OnStdout
		onStderr = opts.OnStderr
		onExit = opts.OnExit
	}

	result, err := gm.machine.Execute(ctx, command, &ExecuteOptions{
		Cwd:      cwd,
		Env:      mergedEnv,
		Timeout:  timeout,
		OnStdout: onStdout,
		OnStderr: onStderr,
	})
	if err != nil {
		return nil, err
	}

	gm.mu.Lock()
	gm.logs = append(gm.logs, LogEntry{
		Command:   command,
		Result:    *result,
		Timestamp: time.Now(),
	})
	gm.mu.Unlock()

	if onExit != nil {
		onExit(result.ExitCode)
	}

	return result, nil
}

// Logs returns a copy of the command execution history.
func (gm *GitMachine) Logs() []LogEntry {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	out := make([]LogEntry, len(gm.logs))
	copy(out, gm.logs)
	return out
}

// --- Internal ---

// whileRunning transparently resumes a paused machine, runs fn, and re-pauses
// if the machine was paused before the call. This allows git operations to work
// even when the machine is paused.
func (gm *GitMachine) whileRunning(ctx context.Context, fn func() error) error {
	wasPaused := gm.machine.State() == StatePaused
	if wasPaused {
		if err := gm.machine.Resume(ctx); err != nil {
			return fmt.Errorf("resume for whileRunning: %w", err)
		}
	}

	fnErr := fn()

	if wasPaused {
		gm.mu.Lock()
		gm.skipAutoCommit = true
		gm.mu.Unlock()

		pauseErr := gm.machine.Pause(ctx)

		gm.mu.Lock()
		gm.skipAutoCommit = false
		gm.mu.Unlock()

		if fnErr != nil {
			return fnErr
		}
		return pauseErr
	}

	return fnErr
}

// autoCommitChanges stages and commits all changes (best-effort).
func (gm *GitMachine) autoCommitChanges(ctx context.Context) {
	_, _ = gm.exec(ctx, `git add -A && git diff --cached --quiet || git commit -m "auto: checkpoint"`)
}

// pushChanges pushes to the origin (best-effort).
func (gm *GitMachine) pushChanges(ctx context.Context) {
	branch := gm.session
	if branch == "" {
		branch = "main"
	}
	_, _ = gm.exec(ctx, fmt.Sprintf("git push origin %s", branch))
}

// exec runs a command in the repo directory.
func (gm *GitMachine) exec(ctx context.Context, command string) (*ExecutionResult, error) {
	return gm.machine.Execute(ctx, command, &ExecuteOptions{Cwd: repoPath})
}

// authURL inserts the token into the repository URL for authentication.
func (gm *GitMachine) authURL() string {
	return strings.Replace(gm.repository, "https://", fmt.Sprintf("https://%s@", gm.token), 1)
}

// emit fires a lifecycle event to the onEvent callback. Errors are swallowed.
func (gm *GitMachine) emit(event string, data map[string]interface{}) {
	if gm.onEventCb == nil {
		return
	}
	func() {
		defer func() {
			// Event callbacks should never crash the machine.
			recover() //nolint:errcheck
		}()
		gm.onEventCb(event, data, gm)
	}()
}

// BoolPtr is a helper to create a *bool for use in GitMachineConfig.AutoCommit.
func BoolPtr(b bool) *bool {
	return &b
}
