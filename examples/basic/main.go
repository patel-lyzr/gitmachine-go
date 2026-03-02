package main

import (
	"context"
	"fmt"
	"log"

	gm "github.com/open-gitagent/gitmachine-go"
)

func main() {
	ctx := context.Background()

	// Create an E2B-backed machine.
	machine := gm.NewE2BMachine(&gm.E2BMachineConfig{
		APIKey:   "e2b_...",
		Template: "base",
		Timeout:  300,
	})

	// Wrap it with GitMachine for git lifecycle management.
	gitMachine := gm.NewGitMachine(gm.GitMachineConfig{
		Machine:    machine,
		Repository: "https://github.com/user/my-agent.git",
		Token:      "ghp_...",
		Session:    "feature-work",
		AutoCommit: gm.BoolPtr(true),
		Env: map[string]string{
			"ANTHROPIC_API_KEY": "sk-...",
		},
		Identity: &gm.GitIdentity{
			Name:  "Agent",
			Email: "agent@example.com",
		},
		OnStart: func(g *gm.GitMachine) error {
			_, err := g.Run(ctx, "npm install -g @anthropic-ai/claude-code", nil)
			return err
		},
		OnEvent: func(event string, data map[string]interface{}, _ *gm.GitMachine) {
			fmt.Printf("[%s] %v\n", event, data)
		},
	})

	// Start: VM up -> repo cloned -> branch checked out -> onStart runs.
	if err := gitMachine.Start(ctx); err != nil {
		log.Fatal(err)
	}

	// Run a command inside the sandbox.
	result, err := gitMachine.Run(ctx, "claude -p 'review this code' --print", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Stdout)

	// Pause: auto-commits -> VM paused.
	if err := gitMachine.Pause(ctx); err != nil {
		log.Fatal(err)
	}

	// ... later ...

	// Resume: VM comes back, repo state intact.
	if err := gitMachine.Resume(ctx); err != nil {
		log.Fatal(err)
	}

	// Run another command.
	result, err = gitMachine.Run(ctx, "claude -p 'continue the review' --print", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Stdout)

	// Stop: auto-commits -> pushes -> VM torn down.
	if err := gitMachine.Stop(ctx); err != nil {
		log.Fatal(err)
	}

	// --- Fire-and-forget pattern using goroutines ---

	// You can run commands concurrently with goroutines:
	//
	//   go func() {
	//       result, err := gitMachine.Run(ctx, "long-running-task", nil)
	//       if err != nil {
	//           log.Println("background task failed:", err)
	//           return
	//       }
	//       fmt.Println("background task done:", result.Stdout)
	//   }()

	fmt.Println("Done. Logs:", len(gitMachine.Logs()), "entries")
}
