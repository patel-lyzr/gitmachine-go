package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	gm "github.com/open-gitagent/gitmachine-go"
)

func handleSandbox(args []string) {
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		sandboxCreate(rest)
	case "list", "ls":
		sandboxList(rest)
	case "status":
		sandboxStatus(rest)
	case "exec":
		sandboxExec(rest)
	case "start":
		sandboxStart(rest)
	case "stop":
		sandboxStop(rest)
	case "remove", "rm":
		sandboxRm(rest)
	case "ssh":
		sandboxSSH(rest)
	case "help", "--help", "-h":
		printSandboxUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown sandbox command: %s\n\n", sub)
		printSandboxUsage()
		os.Exit(1)
	}
}

func sandboxCreate(args []string) {
	// Determine whether first arg is a node ID or a flag.
	nodeIDArg := ""
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		nodeIDArg = args[0]
		rest = args[1:]
	}

	image := ""
	name := ""
	cpus := ""
	memory := ""
	diskSize := ""
	var ports []int

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--image", "-i":
			i++
			if i < len(rest) {
				image = rest[i]
			}
		case "--name", "-n":
			i++
			if i < len(rest) {
				name = rest[i]
			}
		case "--cpus":
			i++
			if i < len(rest) {
				cpus = rest[i]
			}
		case "--memory", "-m":
			i++
			if i < len(rest) {
				memory = rest[i]
			}
		case "--disk", "--disk-size":
			i++
			if i < len(rest) {
				diskSize = rest[i]
			}
		case "--port", "-p":
			i++
			if i < len(rest) {
				p, err := strconv.Atoi(rest[i])
				if err != nil {
					fmt.Fprintf(os.Stderr, "invalid port: %s\n", rest[i])
					os.Exit(1)
				}
				ports = append(ports, p)
			}
		}
	}

	var node *gm.NodeRecord
	if nodeIDArg != "" {
		_, node = resolveNode(nodeIDArg)
	} else {
		node = autoSelectNode()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cloudMachine, err := connectToNode(ctx, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to node %s: %v\n", node.ID, err)
		os.Exit(1)
	}

	config := gm.DockerMachineConfig{
		Image:    image,
		Name:     name,
		CPUs:     cpus,
		Memory:   memory,
		DiskSize: diskSize,
		Ports:    ports,
	}

	dm := gm.NewDockerMachine(cloudMachine, config)

	fmt.Printf("Creating sandbox on node %s...\n", shortID(node.ID))

	if err := dm.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create sandbox: %v\n", err)
		os.Exit(1)
	}

	// Save sandbox state.
	sstate, err := gm.NewSandboxState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save sandbox state: %v\n", err)
	} else {
		finalImage := image
		if finalImage == "" {
			finalImage = "ubuntu:22.04"
		}
		rec := gm.SandboxRecord{
			ID:        dm.ID(),
			Name:      dm.GetName(),
			NodeID:    node.ID,
			Image:     finalImage,
			CPUs:      cpus,
			Memory:    memory,
			DiskSize:  diskSize,
			Ports:     dm.PortMappings(),
			Status:    "running",
			CreatedAt: time.Now(),
		}
		_ = sstate.Add(rec)
	}

	fmt.Println("done!")
	fmt.Println()
	fmt.Printf("  ID:    %s\n", shortID(dm.ID()))
	fmt.Printf("  Name:  %s\n", dm.GetName())
	displayImage := image
	if displayImage == "" {
		displayImage = "ubuntu:22.04"
	}
	fmt.Printf("  Image: %s\n", displayImage)
	if cpus != "" {
		fmt.Printf("  CPUs:  %s\n", cpus)
	}
	if memory != "" {
		fmt.Printf("  Mem:   %s\n", memory)
	}
	if diskSize != "" {
		fmt.Printf("  Disk:  %s\n", diskSize)
	}
	for _, pm := range dm.PortMappings() {
		fmt.Printf("  Port:  %d -> %s\n", pm.ContainerPort, pm.URL)
	}
	fmt.Printf("  Node:  %s\n", shortID(node.ID))
}

func sandboxList(args []string) {
	sstate, err := gm.NewSandboxState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load sandbox state: %v\n", err)
		os.Exit(1)
	}

	var sandboxes []gm.SandboxRecord

	if len(args) > 0 {
		// Filter by node ID.
		_, node := resolveNode(args[0])
		sandboxes = sstate.ForNode(node.ID)
	} else {
		sandboxes = sstate.Sandboxes
	}

	if len(sandboxes) == 0 {
		fmt.Println("No sandboxes. Create one with: gitmachine sandbox create <node-id>")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tNODE\tIMAGE\tSTATUS\tAGE")
	for _, s := range sandboxes {
		age := time.Since(s.CreatedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID(s.ID), s.Name, shortID(s.NodeID), s.Image, s.Status, formatAge(age))
	}
	w.Flush()
}

func sandboxStatus(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine sandbox status <sandbox-id>")
		os.Exit(1)
	}

	sstate, rec := resolveSandbox(args[0])
	_, node := resolveNode(rec.NodeID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cloudMachine, err := connectToNode(ctx, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to node: %v\n", err)
		// Show cached info anyway.
		printSandboxInfo(rec)
		return
	}

	// Get live status from Docker.
	inspectCmd := nodeDockerCmd(ctx, cloudMachine, fmt.Sprintf("docker inspect --format '{{.State.Status}}' %s", rec.ID))
	result, err := cloudMachine.Execute(ctx, inspectCmd, nil)
	if err == nil && result.ExitCode == 0 {
		liveStatus := strings.TrimSpace(result.Stdout)
		if liveStatus != rec.Status {
			rec.Status = liveStatus
			_ = sstate.Update(rec.ID, func(r *gm.SandboxRecord) {
				r.Status = liveStatus
			})
		}
	}

	printSandboxInfo(rec)
}

func sandboxExec(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine sandbox exec <sandbox-id> <command...>")
		os.Exit(1)
	}

	_, rec := resolveSandbox(args[0])
	command := strings.Join(args[1:], " ")

	_, node := resolveNode(rec.NodeID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cloudMachine, err := connectToNode(ctx, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to node: %v\n", err)
		os.Exit(1)
	}

	dm, err := gm.ConnectDockerMachine(cloudMachine, rec.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to sandbox: %v\n", err)
		os.Exit(1)
	}

	result, err := dm.Execute(ctx, command, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Exec failed: %v\n", err)
		os.Exit(1)
	}

	if result.Stdout != "" {
		fmt.Print(result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
	}

	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
}

func sandboxStart(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine sandbox start <sandbox-id>")
		os.Exit(1)
	}

	sstate, rec := resolveSandbox(args[0])
	_, node := resolveNode(rec.NodeID)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cloudMachine, err := connectToNode(ctx, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to node: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Starting sandbox %s...", shortID(rec.ID))

	startCmd := nodeDockerCmd(ctx, cloudMachine, fmt.Sprintf("docker start %s", rec.ID))
	result, err := cloudMachine.Execute(ctx, startCmd, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}
	if result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "\nFailed: %s\n", result.Stderr)
		os.Exit(1)
	}

	_ = sstate.Update(rec.ID, func(r *gm.SandboxRecord) {
		r.Status = "running"
	})
	fmt.Println(" done!")
}

func sandboxStop(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine sandbox stop <sandbox-id>")
		os.Exit(1)
	}

	sstate, rec := resolveSandbox(args[0])
	_, node := resolveNode(rec.NodeID)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cloudMachine, err := connectToNode(ctx, node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to node: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Stopping sandbox %s...", shortID(rec.ID))

	stopCmd := nodeDockerCmd(ctx, cloudMachine, fmt.Sprintf("docker stop %s", rec.ID))
	result, err := cloudMachine.Execute(ctx, stopCmd, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}
	if result.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "\nFailed: %s\n", result.Stderr)
		os.Exit(1)
	}

	_ = sstate.Update(rec.ID, func(r *gm.SandboxRecord) {
		r.Status = "stopped"
	})
	fmt.Println(" done!")
}

func sandboxRm(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine sandbox rm <sandbox-id>")
		os.Exit(1)
	}

	sstate, rec := resolveSandbox(args[0])
	_, node := resolveNode(rec.NodeID)

	fmt.Printf("Removing sandbox %s...", shortID(rec.ID))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Best-effort: try to docker rm on the node, but if the node is
	// unreachable (stopped/terminated) just clean up the local record.
	cloudMachine, err := connectToNode(ctx, node)
	if err == nil {
		rmCmd := nodeDockerCmd(ctx, cloudMachine, fmt.Sprintf("docker rm -f %s", rec.ID))
		_, _ = cloudMachine.Execute(ctx, rmCmd, nil)
	}

	_ = sstate.Remove(rec.ID)
	fmt.Println(" done!")
}

func sandboxSSH(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine sandbox ssh <sandbox-id>")
		os.Exit(1)
	}

	_, rec := resolveSandbox(args[0])
	_, node := resolveNode(rec.NodeID)

	if node.PublicIP == "" {
		fmt.Fprintln(os.Stderr, "Node has no public IP. Is it running?")
		os.Exit(1)
	}

	sshUser := node.SSHUser
	if sshUser == "" {
		sshUser = "ubuntu"
	}

	// SSH into the node with a remote command to docker exec into the container.
	// Use sudo since the SSH session may not have the docker group.
	remoteCmd := fmt.Sprintf("sudo docker exec -it %s /bin/bash || sudo docker exec -it %s /bin/sh", rec.ID, rec.ID)

	sshArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-t"}
	if node.SSHKeyPath != "" {
		sshArgs = append(sshArgs, "-i", node.SSHKeyPath)
	}
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", sshUser, node.PublicIP))
	sshArgs = append(sshArgs, remoteCmd)

	cmd := execCommand("ssh", sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// --- sandbox helpers ---

// autoSelectNode picks the running node with the fewest sandboxes (most available capacity).
func autoSelectNode() *gm.NodeRecord {
	state, err := gm.NewNodeState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load node state: %v\n", err)
		os.Exit(1)
	}

	// Collect running nodes.
	var running []int
	for i := range state.Nodes {
		if state.Nodes[i].Status == "running" && state.Nodes[i].PublicIP != "" {
			running = append(running, i)
		}
	}
	if len(running) == 0 {
		fmt.Fprintln(os.Stderr, "No running nodes. Create one with: gitmachine node create aws")
		os.Exit(1)
	}

	// Count sandboxes per node.
	sstate, _ := gm.NewSandboxState()
	sandboxCount := make(map[string]int)
	if sstate != nil {
		for _, s := range sstate.Sandboxes {
			if s.Status == "running" {
				sandboxCount[s.NodeID]++
			}
		}
	}

	// Pick the running node with the fewest running sandboxes.
	best := running[0]
	for _, idx := range running[1:] {
		if sandboxCount[state.Nodes[idx].ID] < sandboxCount[state.Nodes[best].ID] {
			best = idx
		}
	}

	node := &state.Nodes[best]
	fmt.Printf("Auto-selected node %s (%s, %d running sandbox(es))\n",
		shortID(node.ID), node.InstanceType, sandboxCount[node.ID])
	return node
}

func resolveSandbox(idOrPrefix string) (*gm.SandboxState, *gm.SandboxRecord) {
	sstate, err := gm.NewSandboxState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load sandbox state: %v\n", err)
		os.Exit(1)
	}

	rec := sstate.Find(idOrPrefix)
	if rec == nil {
		rec = sstate.FindByPrefix(idOrPrefix)
	}
	if rec == nil {
		fmt.Fprintf(os.Stderr, "Sandbox %s not found. Run 'gitmachine sandbox list' to see sandboxes.\n", idOrPrefix)
		os.Exit(1)
	}
	return sstate, rec
}

func connectToNode(ctx context.Context, node *gm.NodeRecord) (*gm.CloudMachine, error) {
	provider := providerForNode(node)
	return gm.ConnectCloudMachine(ctx, provider, node.ID)
}

// nodeDockerCmd returns the docker command prefixed with sudo if needed on the node.
func nodeDockerCmd(ctx context.Context, node *gm.CloudMachine, cmd string) string {
	result, err := node.Execute(ctx, "docker version", nil)
	if err != nil || result.ExitCode != 0 {
		return "sudo " + cmd
	}
	return cmd
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func printSandboxInfo(rec *gm.SandboxRecord) {
	fmt.Printf("ID:      %s\n", shortID(rec.ID))
	fmt.Printf("Name:    %s\n", rec.Name)
	fmt.Printf("Node:    %s\n", shortID(rec.NodeID))
	fmt.Printf("Image:   %s\n", rec.Image)
	if rec.CPUs != "" {
		fmt.Printf("CPUs:    %s\n", rec.CPUs)
	}
	if rec.Memory != "" {
		fmt.Printf("Memory:  %s\n", rec.Memory)
	}
	if rec.DiskSize != "" {
		fmt.Printf("Disk:    %s\n", rec.DiskSize)
	}
	for _, pm := range rec.Ports {
		fmt.Printf("Port:    %d -> %s\n", pm.ContainerPort, pm.URL)
	}
	fmt.Printf("Status:  %s\n", rec.Status)
	fmt.Printf("Created: %s (%s ago)\n", rec.CreatedAt.Format(time.RFC3339), formatAge(time.Since(rec.CreatedAt)))
}

func printSandboxUsage() {
	fmt.Println("Usage:")
	fmt.Println("  gitmachine sandbox <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  create <node-id>         Create a new sandbox on a node")
	fmt.Println("  list [node-id]           List sandboxes (optionally filter by node)")
	fmt.Println("  status <sandbox-id>      Show sandbox status")
	fmt.Println("  exec <sandbox-id> <cmd>  Execute a command in a sandbox")
	fmt.Println("  start <sandbox-id>       Start a stopped sandbox")
	fmt.Println("  stop <sandbox-id>        Stop a sandbox")
	fmt.Println("  rm <sandbox-id>          Remove a sandbox")
	fmt.Println("  ssh <sandbox-id>         SSH into a sandbox")
	fmt.Println()
	fmt.Println("Flags (create):")
	fmt.Println("  --image, -i    Docker image (default: ubuntu:22.04)")
	fmt.Println("  --name, -n     Container name (auto-generated if empty)")
	fmt.Println("  --cpus         Number of CPUs (e.g. 1, 0.5, 2)")
	fmt.Println("  --memory, -m   Memory limit (e.g. 512m, 2g)")
	fmt.Println("  --disk         Disk size limit (e.g. 10g, 20g)")
	fmt.Println("  --port, -p     Expose container port (repeatable, e.g. --port 8080 --port 3000)")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  gitmachine sandbox create i-0abc123")
	fmt.Println("  gitmachine sandbox create i-0abc --image node:20 --name my-sandbox")
	fmt.Println("  gitmachine sandbox create i-0abc --cpus 2 --memory 4g --disk 20g")
	fmt.Println("  gitmachine sandbox create i-0abc --image node:20 --port 3000 --port 8080")
	fmt.Println("  gitmachine sandbox list")
	fmt.Println("  gitmachine sandbox exec gm-a3f2b1 whoami")
	fmt.Println("  gitmachine sandbox ssh gm-a3f2b1")
	fmt.Println("  gitmachine sandbox rm gm-a3f2b1")
}
