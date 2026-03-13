package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	gm "github.com/open-gitagent/gitmachine-go"
)

var execCommand = exec.Command

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "node":
		if len(args) == 0 {
			printNodeUsage()
			os.Exit(1)
		}
		handleNode(args)
	case "auth":
		if len(args) == 0 {
			printAuthUsage()
			os.Exit(1)
		}
		handleAuth(args)
	case "sandbox":
		if len(args) == 0 {
			printSandboxUsage()
			os.Exit(1)
		}
		handleSandbox(args)
	case "ui":
		handleUI(args)
	case "version":
		fmt.Println("gitmachine v0.1.0")
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

// ==================== auth ====================

func handleAuth(args []string) {
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "add":
		authAdd(rest)
	case "list", "ls":
		authList()
	case "remove", "rm":
		authRemove(rest)
	case "default":
		authDefault(rest)
	case "help", "--help", "-h":
		printAuthUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown auth command: %s\n\n", sub)
		printAuthUsage()
		os.Exit(1)
	}
}

func authAdd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine auth add <name> <provider>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  gitmachine auth add personal aws")
		fmt.Fprintln(os.Stderr, "  gitmachine auth add work aws")
		os.Exit(1)
	}

	name := args[0]
	provider := strings.ToLower(args[1])

	store, err := gm.NewCredentialStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load credential store: %v\n", err)
		os.Exit(1)
	}

	if existing := store.Get(name); existing != nil {
		fmt.Fprintf(os.Stderr, "Credential %q already exists. Remove it first with: gitmachine auth rm %s\n", name, name)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)
	fields := make(map[string]string)

	switch provider {
	case "aws":
		fmt.Print("AWS Access Key ID: ")
		fields["access_key_id"] = readLine(reader)
		fmt.Print("AWS Secret Access Key: ")
		fields["secret_access_key"] = readLine(reader)
		fmt.Print("Default Region [us-east-1]: ")
		region := readLine(reader)
		if region == "" {
			region = "us-east-1"
		}
		fields["region"] = region
	case "azure":
		fmt.Print("Subscription ID: ")
		fields["subscription_id"] = readLine(reader)
		fmt.Print("Tenant ID: ")
		fields["tenant_id"] = readLine(reader)
		fmt.Print("Client ID: ")
		fields["client_id"] = readLine(reader)
		fmt.Print("Client Secret: ")
		fields["client_secret"] = readLine(reader)
		fmt.Print("Default Region [eastus]: ")
		region := readLine(reader)
		if region == "" {
			region = "eastus"
		}
		fields["region"] = region
	case "gcp":
		fmt.Print("Project ID: ")
		fields["project_id"] = readLine(reader)
		fmt.Print("Credentials JSON file path: ")
		fields["credentials_file"] = readLine(reader)
		fmt.Print("Default Region [us-central1]: ")
		region := readLine(reader)
		if region == "" {
			region = "us-central1"
		}
		fields["region"] = region
	default:
		fmt.Fprintf(os.Stderr, "unsupported provider: %s (supported: aws, azure, gcp)\n", provider)
		os.Exit(1)
	}

	cred := gm.CloudCredential{
		Name:     name,
		Provider: provider,
		Fields:   fields,
	}

	if err := store.Add(cred); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save credential: %v\n", err)
		os.Exit(1)
	}

	defaultLabel := ""
	if stored := store.Get(name); stored != nil && stored.Default {
		defaultLabel = " (default)"
	}
	fmt.Printf("Added %s credential %q%s\n", provider, name, defaultLabel)
}

func authList() {
	store, err := gm.NewCredentialStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load credential store: %v\n", err)
		os.Exit(1)
	}

	if len(store.Credentials) == 0 {
		fmt.Println("No credentials configured. Add one with: gitmachine auth add <name> <provider>")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tREGION\tDEFAULT")
	for _, c := range store.Credentials {
		def := ""
		if c.Default {
			def = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.Provider, c.Fields["region"], def)
	}
	w.Flush()
}

func authRemove(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine auth remove <name>")
		os.Exit(1)
	}

	store, err := gm.NewCredentialStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load credential store: %v\n", err)
		os.Exit(1)
	}

	if err := store.Remove(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed credential %q\n", args[0])
}

func authDefault(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine auth default <name>")
		os.Exit(1)
	}

	store, err := gm.NewCredentialStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load credential store: %v\n", err)
		os.Exit(1)
	}

	if err := store.SetDefault(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Set %q as default\n", args[0])
}

func readLine(reader *bufio.Reader) string {
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

// ==================== node ====================

func handleNode(args []string) {
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "create":
		nodeCreate(rest)
	case "list", "ls":
		nodeList()
	case "status":
		nodeStatus(rest)
	case "stop":
		nodeStop(rest)
	case "start":
		nodeStart(rest)
	case "destroy", "rm":
		nodeDestroy(rest)
	case "ssh":
		nodeSSH(rest)
	case "sync":
		nodeSync()
	case "deploy-agent":
		nodeDeployAgent(rest)
	case "help", "--help", "-h":
		printNodeUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown node command: %s\n\n", sub)
		printNodeUsage()
		os.Exit(1)
	}
}

func nodeCreate(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine node create <provider> [instance-type] [region]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "providers: aws")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "examples:")
		fmt.Fprintln(os.Stderr, "  gitmachine node create aws")
		fmt.Fprintln(os.Stderr, "  gitmachine node create aws t3.micro")
		fmt.Fprintln(os.Stderr, "  gitmachine node create aws t3.micro us-west-2")
		fmt.Fprintln(os.Stderr, "  gitmachine node create aws --type t3.large --region eu-west-1")
		fmt.Fprintln(os.Stderr, "  gitmachine node create aws --account work")
		os.Exit(1)
	}

	providerName := strings.ToLower(args[0])
	rest := args[1:]

	// Parse flags and positional args.
	instanceType := ""
	region := ""
	ami := ""
	sshUser := ""
	name := ""
	account := "" // --account flag to pick a specific credential

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--type", "-t":
			i++
			if i < len(rest) {
				instanceType = rest[i]
			}
		case "--region", "-r":
			i++
			if i < len(rest) {
				region = rest[i]
			}
		case "--ami":
			i++
			if i < len(rest) {
				ami = rest[i]
			}
		case "--user":
			i++
			if i < len(rest) {
				sshUser = rest[i]
			}
		case "--name", "-n":
			i++
			if i < len(rest) {
				name = rest[i]
			}
		case "--account", "-a":
			i++
			if i < len(rest) {
				account = rest[i]
			}
		default:
			if instanceType == "" {
				instanceType = rest[i]
			} else if region == "" {
				region = rest[i]
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch providerName {
	case "aws":
		createAWS(ctx, instanceType, region, ami, sshUser, name, account)
	default:
		fmt.Fprintf(os.Stderr, "unsupported provider: %s (supported: aws)\n", providerName)
		os.Exit(1)
	}
}

// resolveAWSConfig builds EC2MachineConfig from stored credentials or env vars.
func resolveAWSConfig(account, instanceType, region, ami, sshUser string) *gm.EC2MachineConfig {
	config := &gm.EC2MachineConfig{}

	// Try to load from credential store.
	store, err := gm.NewCredentialStore()
	if err == nil {
		var cred *gm.CloudCredential
		if account != "" {
			cred = store.Get(account)
			if cred == nil {
				fmt.Fprintf(os.Stderr, "Credential %q not found. Run 'gitmachine auth list' to see available credentials.\n", account)
				os.Exit(1)
			}
			if cred.Provider != "aws" {
				fmt.Fprintf(os.Stderr, "Credential %q is for %s, not aws\n", account, cred.Provider)
				os.Exit(1)
			}
		} else {
			cred = store.GetDefault("aws")
		}

		if cred != nil {
			config.AccessKeyID = cred.Fields["access_key_id"]
			config.SecretAccessKey = cred.Fields["secret_access_key"]
			if region == "" {
				region = cred.Fields["region"]
			}
			fmt.Printf("Using credential: %s\n", cred.Name)
		}
	}
	// If no stored creds, NewEC2Provider falls back to env vars.

	if instanceType != "" {
		config.InstanceType = instanceType
	}
	if region != "" {
		config.Region = region
	}
	if ami != "" {
		config.AMI = ami
	}
	if sshUser != "" {
		config.SSHUser = sshUser
	}

	return config
}

func createAWS(ctx context.Context, instanceType, region, ami, sshUser, name, account string) {
	config := resolveAWSConfig(account, instanceType, region, ami, sshUser)

	if name != "" {
		config.Tags = map[string]string{"Name": name}
	}

	provider := gm.NewEC2Provider(config)
	machine := gm.NewCloudMachine(provider)

	fmt.Print("Launching EC2 instance...")

	if err := machine.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed to create node: %v\n", err)
		os.Exit(1)
	}

	// Resolve final values for display.
	if instanceType == "" {
		instanceType = "t3.medium"
	}
	if region == "" {
		region = config.Region
		if region == "" {
			region = os.Getenv("AWS_REGION")
			if region == "" {
				region = "us-east-1"
			}
		}
	}

	// Save SSH key and state.
	state, err := gm.NewNodeState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nWarning: could not save state: %v\n", err)
	} else {
		sshUserFinal, privateKeyPEM := provider.SSHConfig()
		if sshUser != "" {
			sshUserFinal = sshUser
		}

		keyPath, keyErr := state.SaveKey(machine.ID(), privateKeyPEM)
		if keyErr != nil {
			fmt.Fprintf(os.Stderr, "\nWarning: could not save SSH key: %v\n", keyErr)
		}

		nodeName := name
		if nodeName == "" {
			nodeName = "gitmachine"
		}
		record := gm.NodeRecord{
			ID:           machine.ID(),
			Provider:     "aws",
			InstanceType: instanceType,
			Region:       region,
			PublicIP:     machine.GetPublicIP(),
			SSHUser:      sshUserFinal,
			SSHKeyPath:   keyPath,
			Status:       "running",
			CreatedAt:    time.Now(),
			Tags:         map[string]string{"Name": nodeName},
		}

		// Store AWS resource names for cleanup on destroy.
		record.SSHKeyName = provider.CreatedKeyName()
		record.SecurityGrp = provider.CreatedSGID()

		_ = state.Add(record)
	}

	fmt.Println(" done!")
	fmt.Println()
	fmt.Printf("  ID:       %s\n", machine.ID())
	fmt.Printf("  IP:       %s\n", machine.GetPublicIP())
	fmt.Printf("  Type:     %s\n", instanceType)
	fmt.Printf("  Region:   %s\n", region)
	fmt.Printf("  Provider: aws\n")

	// Auto-deploy agent to the new node.
	node := state.Find(machine.ID())
	if node != nil {
		fmt.Print("\nDeploying agent...")
		if err := deployAgentToNode(node); err != nil {
			fmt.Fprintf(os.Stderr, " failed: %v\n", err)
			fmt.Println("  You can retry with: gitmachine node deploy-agent", machine.ID())
		} else {
			fmt.Println(" done!")
			fmt.Printf("  Agent: http://%s:9420\n", node.PublicIP)
		}
	}
}

func nodeList() {
	state, err := gm.NewNodeState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}

	if len(state.Nodes) == 0 {
		fmt.Println("No nodes. Create one with: gitmachine node create <provider>")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPROVIDER\tTYPE\tREGION\tIP\tSTATUS\tAGE")
	for _, n := range state.Nodes {
		age := time.Since(n.CreatedAt).Truncate(time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.ID, n.Provider, n.InstanceType, n.Region, n.PublicIP, n.Status, formatAge(age))
	}
	w.Flush()
}

func nodeStatus(args []string) {
	state, err := gm.NewNodeState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}

	if len(args) == 0 {
		if len(state.Nodes) == 0 {
			fmt.Println("No nodes.")
			return
		}
		ctx := context.Background()
		for i := range state.Nodes {
			refreshNodeStatus(ctx, &state.Nodes[i])
		}
		nodeList()
		return
	}

	id := args[0]
	node := state.Find(id)
	if node == nil {
		node = findByPrefix(state, id)
	}
	if node == nil {
		fmt.Fprintf(os.Stderr, "Node %s not found\n", id)
		os.Exit(1)
	}

	ctx := context.Background()
	refreshNodeStatus(ctx, node)
	_ = state.Update(node.ID, func(n *gm.NodeRecord) {
		n.Status = node.Status
		n.PublicIP = node.PublicIP
	})

	fmt.Printf("ID:       %s\n", node.ID)
	fmt.Printf("Provider: %s\n", node.Provider)
	fmt.Printf("Type:     %s\n", node.InstanceType)
	fmt.Printf("Region:   %s\n", node.Region)
	fmt.Printf("IP:       %s\n", node.PublicIP)
	fmt.Printf("Status:   %s\n", node.Status)
	fmt.Printf("Created:  %s (%s ago)\n", node.CreatedAt.Format(time.RFC3339), formatAge(time.Since(node.CreatedAt)))
}

func nodeStop(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine node stop <node-id>")
		os.Exit(1)
	}

	state, node := resolveNode(args[0])
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	provider := providerForNode(node)
	fmt.Printf("Stopping %s...", node.ID)

	if err := provider.StopInstance(ctx, node.ID); err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}

	_ = state.Update(node.ID, func(n *gm.NodeRecord) {
		n.Status = "stopped"
	})
	fmt.Println(" done!")
}

func nodeStart(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine node start <node-id>")
		os.Exit(1)
	}

	state, node := resolveNode(args[0])
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	provider := providerForNode(node)
	fmt.Printf("Starting %s...", node.ID)

	inst, err := provider.StartInstance(ctx, node.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}

	_ = state.Update(node.ID, func(n *gm.NodeRecord) {
		n.Status = "running"
		n.PublicIP = inst.PublicIP
	})
	fmt.Println(" done!")
	fmt.Printf("  IP: %s\n", inst.PublicIP)

	// Auto-deploy/restart agent.
	node.PublicIP = inst.PublicIP
	fmt.Print("Starting agent...")
	if err := deployAgentToNode(node); err != nil {
		fmt.Fprintf(os.Stderr, " failed: %v\n", err)
	} else {
		fmt.Println(" done!")
	}
}

func nodeDestroy(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine node destroy <node-id>")
		os.Exit(1)
	}

	state, node := resolveNode(args[0])
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	provider := providerForNode(node)
	fmt.Printf("Destroying %s...", node.ID)

	// Check if this is the last node (to decide whether to clean up shared SG).
	isLastNode := len(state.Nodes) <= 1
	sgID := node.SecurityGrp

	if err := provider.Terminate(ctx, node.ID); err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}

	state.RemoveKey(node.ID)
	_ = state.Remove(node.ID)

	// Delete shared security group if no more nodes remain.
	if isLastNode && sgID != "" {
		if ec2p, ok := provider.(*gm.EC2Provider); ok {
			_ = ec2p.DeleteSecurityGroup(ctx, node.ID)
		}
	}

	// Clean up orphaned sandbox records for this node.
	if sstate, err := gm.NewSandboxState(); err == nil {
		_ = sstate.RemoveForNode(node.ID)
	}

	fmt.Println(" done!")
}

func nodeSSH(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine node ssh <node-id> [command]")
		os.Exit(1)
	}

	_, node := resolveNode(args[0])

	if node.PublicIP == "" {
		fmt.Fprintln(os.Stderr, "Node has no public IP. Is it running?")
		os.Exit(1)
	}

	sshUser := node.SSHUser
	if sshUser == "" {
		sshUser = "ubuntu"
	}

	sshArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"}
	if node.SSHKeyPath != "" {
		sshArgs = append(sshArgs, "-i", node.SSHKeyPath)
	}
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", sshUser, node.PublicIP))

	if len(args) > 1 {
		sshArgs = append(sshArgs, strings.Join(args[1:], " "))
	}

	cmd := execCommand("ssh", sshArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// deployAgentToNode builds, uploads, and starts the agent on a node.
// It's called automatically after node create and node start.
func deployAgentToNode(node *gm.NodeRecord) error {
	if node.PublicIP == "" {
		return fmt.Errorf("node has no public IP")
	}
	if node.SSHKeyPath == "" {
		return fmt.Errorf("no SSH key for node %s — destroy and recreate, or place key at ~/.gitmachine/keys/%s.pem", node.ID, node.ID)
	}

	sshUser := node.SSHUser
	if sshUser == "" {
		sshUser = "ubuntu"
	}

	// Wait for SSH to become available (fresh EC2 instances take 30-60s).
	sshBase := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5"}
	if node.SSHKeyPath != "" {
		sshBase = append(sshBase, "-i", node.SSHKeyPath)
	}
	dest := fmt.Sprintf("%s@%s", sshUser, node.PublicIP)
	for attempt := 1; attempt <= 12; attempt++ {
		waitArgs := append(append([]string{}, sshBase...), dest, "echo ready")
		waitCmd := execCommand("ssh", waitArgs...)
		if out, err := waitCmd.CombinedOutput(); err == nil && strings.Contains(string(out), "ready") {
			break
		}
		if attempt == 12 {
			return fmt.Errorf("SSH not available after 60s")
		}
		log.Printf("agent-deploy: waiting for SSH on %s (attempt %d/12)...", node.PublicIP, attempt)
		time.Sleep(5 * time.Second)
	}

	// Check if agent is already running.
	checkArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5"}
	if node.SSHKeyPath != "" {
		checkArgs = append(checkArgs, "-i", node.SSHKeyPath)
	}
	checkArgs = append(checkArgs, fmt.Sprintf("%s@%s", sshUser, node.PublicIP), "curl -sf --connect-timeout 2 http://localhost:9420/health")
	checkCmd := execCommand("ssh", checkArgs...)
	if out, err := checkCmd.CombinedOutput(); err == nil && strings.Contains(string(out), "ok") {
		return nil // already running
	}

	// Check if binary exists on node, just start it.
	startArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5"}
	if node.SSHKeyPath != "" {
		startArgs = append(startArgs, "-i", node.SSHKeyPath)
	}
	startScript := "test -f /usr/local/bin/gitmachine-agent && " +
		"{ sudo systemctl start gitmachine-agent 2>/dev/null || sudo nohup /usr/local/bin/gitmachine-agent > /var/log/gitmachine-agent.log 2>&1 &; sleep 1; echo started; } || echo missing"
	startArgs = append(startArgs, fmt.Sprintf("%s@%s", sshUser, node.PublicIP), startScript)
	startCmd := execCommand("ssh", startArgs...)
	if out, err := startCmd.CombinedOutput(); err == nil && strings.Contains(string(out), "started") {
		return nil
	}

	// Need to build and upload.
	agentBinary := "/tmp/gitmachine-agent-linux"
	daemonBinary := "/tmp/gitmachine-sandbox-daemon-linux"

	// Build both binaries for linux/amd64.
	log.Printf("agent-deploy: building binaries...")
	buildAgent := execCommand("go", "build", "-o", agentBinary, "./cmd/gitmachine-agent")
	buildAgent.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	buildDaemon := execCommand("go", "build", "-o", daemonBinary, "./cmd/gitmachine-sandbox-daemon")
	buildDaemon.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")

	if out, err := buildAgent.CombinedOutput(); err != nil {
		return fmt.Errorf("build agent: %s (%w)", string(out), err)
	}
	if out, err := buildDaemon.CombinedOutput(); err != nil {
		return fmt.Errorf("build sandbox-daemon: %s (%w)", string(out), err)
	}
	log.Printf("agent-deploy: binaries built, uploading via SCP...")

	// SCP upload both binaries.
	sshOpts := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"}
	if node.SSHKeyPath != "" {
		sshOpts = append(sshOpts, "-i", node.SSHKeyPath)
	}

	scpArgs := append(append([]string{}, sshOpts...), agentBinary, daemonBinary, dest+":/tmp/")
	scpCmd := execCommand("scp", scpArgs...)
	if out, err := scpCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("scp: %s (%w)", string(out), err)
	}

	// Install Docker (if needed) + agent + sandbox-daemon as a systemd service.
	installScript := `#!/bin/bash
set -e

# Install Docker if not present.
if ! command -v docker &>/dev/null; then
  echo "Installing Docker..."
  curl -fsSL https://get.docker.com | sudo sh
  sudo usermod -aG docker $USER
  echo "Docker installed."
fi

# Install agent binary.
sudo mv /tmp/gitmachine-agent-linux /usr/local/bin/gitmachine-agent
sudo chmod +x /usr/local/bin/gitmachine-agent
sudo pkill gitmachine-agent 2>/dev/null || true

# Install sandbox daemon binary (used inside containers).
sudo mv /tmp/gitmachine-sandbox-daemon-linux /usr/local/bin/gitmachine-sandbox-daemon
sudo chmod +x /usr/local/bin/gitmachine-sandbox-daemon

# Create systemd service.
sudo tee /etc/systemd/system/gitmachine-agent.service > /dev/null << 'UNIT'
[Unit]
Description=GitMachine Agent
After=network.target docker.service

[Service]
ExecStart=/usr/local/bin/gitmachine-agent
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
UNIT

sudo systemctl daemon-reload
sudo systemctl enable gitmachine-agent
sudo systemctl restart gitmachine-agent
echo "Agent started."`

	sshArgs := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"}
	if node.SSHKeyPath != "" {
		sshArgs = append(sshArgs, "-i", node.SSHKeyPath)
	}
	sshArgs = append(sshArgs, fmt.Sprintf("%s@%s", sshUser, node.PublicIP), installScript)
	log.Printf("agent-deploy: running install script on %s...", node.PublicIP)
	sshCmd := execCommand("ssh", sshArgs...)
	if out, err := sshCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("install: %s (%w)", string(out), err)
	}

	// Wait briefly for the service to start.
	time.Sleep(2 * time.Second)
	log.Printf("agent-deploy: install complete on %s", node.PublicIP)
	return nil
}

func nodeDeployAgent(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gitmachine node deploy-agent <node-id>")
		os.Exit(1)
	}

	_, node := resolveNode(args[0])

	fmt.Print("Deploying agent...")
	if err := deployAgentToNode(node); err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(" done!")
	fmt.Printf("Agent URL: http://%s:9420\n", node.PublicIP)
}

func nodeSync() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Print("Syncing nodes from AWS...")
	result, state, err := gm.SyncNodesFromAWSWithCreds(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nFailed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(" done!")
	if len(result.Added) > 0 {
		fmt.Printf("  Added:   %v\n", result.Added)
	}
	if len(result.Updated) > 0 {
		fmt.Printf("  Updated: %v\n", result.Updated)
	}
	if len(result.Added) == 0 && len(result.Updated) == 0 {
		fmt.Println("  Everything up to date.")
	}

	// Show the current node list.
	if state != nil && len(state.Nodes) > 0 {
		fmt.Println()
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tPROVIDER\tTYPE\tREGION\tIP\tSTATUS\tAGE")
		for _, n := range state.Nodes {
			age := time.Since(n.CreatedAt).Truncate(time.Second)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				n.ID, n.Provider, n.InstanceType, n.Region, n.PublicIP, n.Status, formatAge(age))
		}
		w.Flush()
	}
}

// ==================== helpers ====================

func resolveNode(idOrPrefix string) (*gm.NodeState, *gm.NodeRecord) {
	state, err := gm.NewNodeState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load state: %v\n", err)
		os.Exit(1)
	}

	node := state.Find(idOrPrefix)
	if node == nil {
		node = findByPrefix(state, idOrPrefix)
	}
	if node == nil {
		fmt.Fprintf(os.Stderr, "Node %s not found. Run 'gitmachine node list' to see nodes.\n", idOrPrefix)
		os.Exit(1)
	}
	return state, node
}

func findByPrefix(state *gm.NodeState, prefix string) *gm.NodeRecord {
	var match *gm.NodeRecord
	for i := range state.Nodes {
		if strings.HasPrefix(state.Nodes[i].ID, prefix) {
			if match != nil {
				return nil // ambiguous
			}
			match = &state.Nodes[i]
		}
	}
	return match
}

func providerForNode(node *gm.NodeRecord) gm.CloudProvider {
	// Load stored credentials for this provider.
	store, _ := gm.NewCredentialStore()

	switch node.Provider {
	case "aws":
		config := &gm.EC2MachineConfig{Region: node.Region}
		if store != nil {
			if cred := store.GetDefault("aws"); cred != nil {
				config.AccessKeyID = cred.Fields["access_key_id"]
				config.SecretAccessKey = cred.Fields["secret_access_key"]
			}
		}
		// Load stored private key so SSH works on reconnect.
		if node.SSHKeyPath != "" {
			if keyData, err := os.ReadFile(node.SSHKeyPath); err == nil {
				config.PrivateKeyPEM = string(keyData)
			}
		}
		if node.SSHUser != "" {
			config.SSHUser = node.SSHUser
		}
		p := gm.NewEC2Provider(config)
		// Restore resource names so Terminate can clean up key pairs and SGs.
		p.SetCleanupInfo(node.SSHKeyName, node.SecurityGrp)
		return p
	default:
		fmt.Fprintf(os.Stderr, "unsupported provider: %s\n", node.Provider)
		os.Exit(1)
		return nil
	}
}

func refreshNodeStatus(ctx context.Context, node *gm.NodeRecord) {
	provider := providerForNode(node)
	inst, err := provider.Describe(ctx, node.ID)
	if err != nil {
		node.Status = "unknown"
		return
	}
	node.PublicIP = inst.PublicIP
	if inst.PublicIP != "" {
		node.Status = "running"
	}
}

func formatAge(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// ==================== usage ====================

func printUsage() {
	fmt.Println("gitmachine - Git-aware cloud machine orchestration")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  gitmachine <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  auth       Manage cloud credentials")
	fmt.Println("  node       Manage cloud compute nodes")
	fmt.Println("  sandbox    Manage Docker sandboxes on nodes")
	fmt.Println("  ui         Open web dashboard")
	fmt.Println("  version    Show version")
	fmt.Println("  help       Show this help")
}

func printAuthUsage() {
	fmt.Println("Usage:")
	fmt.Println("  gitmachine auth <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  add <name> <provider>    Add cloud credentials (interactive)")
	fmt.Println("  list                     List all credentials")
	fmt.Println("  remove <name>            Remove a credential")
	fmt.Println("  default <name>           Set default credential for a provider")
	fmt.Println()
	fmt.Println("Providers: aws, azure, gcp")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  gitmachine auth add personal aws")
	fmt.Println("  gitmachine auth add work aws")
	fmt.Println("  gitmachine auth default work")
	fmt.Println("  gitmachine auth list")
	fmt.Println()
	fmt.Println("Then use with node create:")
	fmt.Println("  gitmachine node create aws                  # uses default aws credential")
	fmt.Println("  gitmachine node create aws --account work   # uses 'work' credential")
}

func printNodeUsage() {
	fmt.Println("Usage:")
	fmt.Println("  gitmachine node <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  create <provider> [type] [region]    Launch a new node")
	fmt.Println("  list                                 List all nodes")
	fmt.Println("  sync                                 Discover & sync nodes from AWS")
	fmt.Println("  status [node-id]                     Show node status")
	fmt.Println("  stop <node-id>                       Stop a node (preserves disk)")
	fmt.Println("  start <node-id>                      Start a stopped node")
	fmt.Println("  destroy <node-id>                    Terminate and remove a node")
	fmt.Println("  ssh <node-id>                        SSH into a node")
	fmt.Println()
	fmt.Println("Providers:")
	fmt.Println("  aws        Amazon EC2")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --type, -t       Instance type (e.g. t3.micro)")
	fmt.Println("  --region, -r     Region (e.g. us-west-2)")
	fmt.Println("  --account, -a    Use a specific credential (from 'gitmachine auth list')")
	fmt.Println("  --name, -n       Name tag for the instance")
	fmt.Println("  --ami            Custom AMI ID")
	fmt.Println("  --user           SSH username")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  gitmachine node create aws")
	fmt.Println("  gitmachine node create aws t3.micro us-west-2")
	fmt.Println("  gitmachine node create aws --account personal --type t3.large")
	fmt.Println("  gitmachine node list")
	fmt.Println("  gitmachine node ssh i-0abc123")
	fmt.Println("  gitmachine node destroy i-0abc")
}
