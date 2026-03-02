# GitMachine (Go)

A Go package for running git-aware sandboxed virtual machines. Clone a repo into an isolated VM, run commands, and auto-commit results back -- with branch-based session persistence.

GitMachine is the infrastructure layer for [GitAgent](https://github.com/open-gitagent/gitagent). It handles VM lifecycle + git operations so agent orchestration layers can focus on what matters.

## Install

```bash
go get github.com/open-gitagent/gitmachine-go
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"

    gm "github.com/open-gitagent/gitmachine-go"
)

func main() {
    ctx := context.Background()

    machine := gm.NewE2BMachine(&gm.E2BMachineConfig{
        APIKey: "e2b_...",
    })

    gitMachine := gm.NewGitMachine(gm.GitMachineConfig{
        Machine:    machine,
        Repository: "https://github.com/user/my-agent.git",
        Token:      "ghp_...",
        Session:    "feature-work",
        AutoCommit: gm.BoolPtr(true),
        Env:        map[string]string{"ANTHROPIC_API_KEY": "sk-..."},
        OnStart: func(g *gm.GitMachine) error {
            _, err := g.Run(ctx, "npm install -g @anthropic-ai/claude-code", nil)
            return err
        },
        OnEvent: func(event string, data map[string]interface{}, _ *gm.GitMachine) {
            fmt.Printf("[%s] %v\n", event, data)
        },
    })

    if err := gitMachine.Start(ctx); err != nil {
        log.Fatal(err)
    }

    result, err := gitMachine.Run(ctx, "claude -p 'review this code' --print", nil)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.Stdout)

    if err := gitMachine.Pause(ctx); err != nil {
        log.Fatal(err)
    }

    // ... later ...

    if err := gitMachine.Resume(ctx); err != nil {
        log.Fatal(err)
    }

    result, err = gitMachine.Run(ctx, "claude -p 'continue the review' --print", nil)
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(result.Stdout)

    if err := gitMachine.Stop(ctx); err != nil {
        log.Fatal(err)
    }
}
```

### Fire-and-Forget (Goroutines)

```go
// Run a long task in the background using a goroutine.
go func() {
    result, err := gitMachine.Run(ctx, "long-running-task", nil)
    if err != nil {
        log.Println("background task failed:", err)
        return
    }
    fmt.Println("background result:", result.Stdout)
}()
```

## Architecture

```
+------------------------------------------+
|              GitMachine                   |
|  git clone / commit / push / sessions    |
|  auto-commit on pause/stop               |
+------------------------------------------+
|           Machine (interface)             |
|  Start / Pause / Resume / Stop           |
|  Execute / ReadFile / WriteFile          |
+------------------------------------------+
|             E2BMachine                    |
|  E2B Sandbox REST API                    |
|  Full API access via GetSandboxID()      |
+------------------------------------------+
```

**Machine** -- Go interface. Any VM provider (E2B, Fly, bare metal) implements this.

**E2BMachine** -- Concrete implementation using [E2B](https://e2b.dev) sandboxes via REST API. Exposes additional E2B-specific methods.

**GitMachine** -- Wraps any Machine with git lifecycle management. Clones a repo on start, auto-commits on pause/stop, persists sessions as branches.

## API

### `E2BMachine`

```go
machine := gm.NewE2BMachine(&gm.E2BMachineConfig{
    APIKey:   "...",              // defaults to E2B_API_KEY env var
    Template: "base",            // sandbox template, default "base"
    Timeout:  300,               // seconds, default 300
    Envs:     map[string]string{},
    Metadata: map[string]string{},
})
```

Implements the `Machine` interface and exposes additional E2B-specific methods:

| Method | Description |
|--------|-------------|
| `GetSandboxID()` | Current sandbox ID |
| `SetTimeout(ctx, seconds)` | Extend sandbox timeout |
| `GetInfo(ctx)` | Sandbox metadata |
| `MakeDir(ctx, path)` | Create directory |
| `Remove(ctx, path)` | Remove file/directory |
| `Exists(ctx, path)` | Check if path exists |
| `IsRunning(ctx)` | Check sandbox status |
| `GetHost(port)` | Get host address for a sandbox port |

Connect to an existing sandbox:

```go
machine, err := gm.ConnectE2BMachine(ctx, "sandbox-id", &gm.E2BMachineConfig{
    APIKey: "e2b_...",
})
```

### `GitMachine`

```go
gitMachine := gm.NewGitMachine(gm.GitMachineConfig{
    Machine:    machine,                   // VM provider (required)
    Repository: "https://...",             // git repo URL (required)
    Token:      "ghp_...",                 // PAT for git auth (required)
    Identity:   &gm.GitIdentity{...},     // git user, default GitMachine
    OnStart:    func(g) error { ... },     // called after clone
    OnPause:    func(g) error { ... },     // called before pause
    OnResume:   func(g) error { ... },     // called after resume
    OnEnd:      func(g) error { ... },     // called before stop
    Env:        map[string]string{...},    // env vars for all commands
    Session:    "branch-name",             // branch for persistence
    AutoCommit: gm.BoolPtr(true),          // default true
    OnEvent:    func(event, data, g) {},   // lifecycle events
})
```

Reconnect to an already-running machine:

```go
gm, err := gm.ConnectGitMachine(machine, config)
```

#### Lifecycle

| Method | Description |
|--------|-------------|
| `Start(ctx)` | Start VM, clone repo, checkout session branch, run OnStart |
| `Pause(ctx)` | Auto-commit (if enabled), pause VM |
| `Resume(ctx)` | Resume VM |
| `Stop(ctx)` | Auto-commit, push, run OnEnd, kill VM |

#### Git Operations

| Method | Description |
|--------|-------------|
| `Diff(ctx)` | Git diff against HEAD |
| `Commit(ctx, message)` | Stage all + commit, returns SHA or empty if clean |
| `Push(ctx)` | Push to origin |
| `Pull(ctx)` | Pull from origin |
| `Hash(ctx)` | Current HEAD SHA |

All git operations use the **whileRunning** pattern -- if the machine is paused, they transparently resume, do the work, and re-pause.

#### Runtime

| Method | Description |
|--------|-------------|
| `Run(ctx, command, opts)` | Execute a command in the sandbox |
| `Update(env, onUpdate)` | Update environment variables |
| `Logs()` | Get command execution history |

#### Properties

| Method | Description |
|--------|-------------|
| `State()` | Current `MachineState` (idle/running/paused/stopped) |
| `Path()` | Repo path inside the VM |
| `ID()` | Underlying machine ID |

### Events

The `OnEvent` callback receives lifecycle events:

| Event | Data |
|-------|------|
| `started` | `session`, `repoPath` |
| `paused` | |
| `resumed` | |
| `stopping` | |
| `stopped` | |
| `committed` | `sha`, `message` |
| `pushed` | |
| `pulled` | |
| `reconnected` | `id` |

## Sessions

Sessions map to git branches. When you specify a `Session`:

1. **Start** -- checks out the branch (creates it if new)
2. **Pause** -- auto-commits all changes to the branch
3. **Stop** -- auto-commits and pushes to the branch
4. **Next run** -- resumes from the same branch state

This gives you persistent, resumable agent sessions backed by git.

## Extending

### Custom Machine Provider

```go
package myprovider

import (
    "context"

    gm "github.com/open-gitagent/gitmachine-go"
)

type FlyMachine struct {
    // ...
}

func (m *FlyMachine) ID() string                    { /* ... */ return "" }
func (m *FlyMachine) State() gm.MachineState        { /* ... */ return gm.StateIdle }
func (m *FlyMachine) Start(ctx context.Context) error { /* ... */ return nil }
func (m *FlyMachine) Pause(ctx context.Context) error { /* ... */ return nil }
func (m *FlyMachine) Resume(ctx context.Context) error { /* ... */ return nil }
func (m *FlyMachine) Stop(ctx context.Context) error  { /* ... */ return nil }

func (m *FlyMachine) Execute(ctx context.Context, command string, opts *gm.ExecuteOptions) (*gm.ExecutionResult, error) {
    /* ... */
    return nil, nil
}

func (m *FlyMachine) ReadFile(ctx context.Context, path string) (string, error) {
    /* ... */
    return "", nil
}

func (m *FlyMachine) WriteFile(ctx context.Context, path string, content []byte) error {
    /* ... */
    return nil
}

func (m *FlyMachine) ListFiles(ctx context.Context, path string) ([]string, error) {
    /* ... */
    return nil, nil
}

// Compile-time interface check.
var _ gm.Machine = (*FlyMachine)(nil)
```

Then use it with GitMachine:

```go
gitMachine := gm.NewGitMachine(gm.GitMachineConfig{
    Machine:    &FlyMachine{},
    Repository: "https://github.com/user/repo.git",
    Token:      "ghp_...",
})
```

## Thread Safety

GitMachine uses `sync.Mutex` internally to protect shared state. All public methods are safe for concurrent use from multiple goroutines.

## License

MIT
