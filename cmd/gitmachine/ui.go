package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	gm "github.com/open-gitagent/gitmachine-go"
	"golang.org/x/crypto/ssh"
)

//go:embed ui.html
var uiHTML embed.FS

type nodeResponse struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	InstanceType string `json:"instance_type"`
	Region       string `json:"region"`
	PublicIP     string `json:"public_ip"`
	Status       string `json:"status"`
	AgentStatus  string `json:"agent_status"`
	Age          string `json:"age"`
	CreatedAt    string `json:"created_at"`
}

type sandboxResponse struct {
	ID           string           `json:"id"`
	ShortID      string           `json:"short_id"`
	Name         string           `json:"name"`
	NodeID       string           `json:"node_id"`
	NodeShort    string           `json:"node_short"`
	Image        string           `json:"image"`
	CPUs         string           `json:"cpus,omitempty"`
	Memory       string           `json:"memory,omitempty"`
	DiskSize     string           `json:"disk_size,omitempty"`
	Ports        []gm.PortMapping `json:"ports,omitempty"`
	Status       string           `json:"status"`
	DaemonStatus string           `json:"daemon_status"`
	Age          string           `json:"age"`
	CreatedAt    string           `json:"created_at"`
}

type credentialResponse struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
	Default  bool   `json:"default"`
}

func handleUI(args []string) {
	port := "8420"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port", "-p":
			i++
			if i < len(args) {
				port = args[i]
			}
		}
	}

	mux := http.NewServeMux()

	// API routes.
	mux.HandleFunc("GET /api/nodes", apiListNodes)
	mux.HandleFunc("POST /api/nodes/refresh-all", apiRefreshAllNodes)
	mux.HandleFunc("POST /api/nodes/{id}/refresh", apiRefreshNode)
	mux.HandleFunc("POST /api/nodes/{id}/stop", apiStopNode)
	mux.HandleFunc("POST /api/nodes/{id}/start", apiStartNode)
	mux.HandleFunc("DELETE /api/nodes/{id}", apiDestroyNode)
	mux.HandleFunc("POST /api/nodes", apiCreateNode)
	mux.HandleFunc("GET /api/credentials", apiListCredentials)
	mux.HandleFunc("GET /api/nodes/{id}/ssh", apiSSHWebSocket)
	mux.HandleFunc("POST /api/nodes/{id}/deploy-agent", apiDeployAgent)

	// Sandbox API routes.
	mux.HandleFunc("GET /api/sandboxes", apiListSandboxes)
	mux.HandleFunc("POST /api/sandboxes", apiCreateSandbox)
	mux.HandleFunc("DELETE /api/sandboxes/{id}", apiDestroySandbox)
	mux.HandleFunc("GET /api/sandboxes/{id}", apiGetSandbox)
	mux.HandleFunc("POST /api/sandboxes/{id}/exec", apiExecSandbox)
	mux.HandleFunc("POST /api/sandboxes/{id}/start", apiStartSandbox)
	mux.HandleFunc("POST /api/sandboxes/{id}/stop", apiStopSandbox)
	mux.HandleFunc("GET /api/sandboxes/{id}/ssh", apiSandboxSSHWebSocket)

	// Serve embedded HTML (catch-all, must be registered last).
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		data, _ := uiHTML.ReadFile("ui.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Write(data)
	})

	url := fmt.Sprintf("http://localhost:%s", port)
	fmt.Printf("Dashboard: %s\n", url)
	openBrowser(url)

	handler := logMiddleware(mux)
	if err := http.ListenAndServe(":"+port, handler); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}

// logMiddleware logs each HTTP request with method, path, status, and duration.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &loggingResponseWriter{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(lw, r)
		duration := time.Since(start)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, lw.statusCode, duration.Round(time.Millisecond))
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (lw *loggingResponseWriter) WriteHeader(code int) {
	lw.statusCode = code
	lw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker so WebSocket upgrades work through the middleware.
func (lw *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := lw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
}

func openBrowser(url string) {
	var cmd string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "linux":
		cmd = "xdg-open"
	default:
		return
	}
	_ = exec.Command(cmd, url).Start()
}

// --- API handlers ---

func apiListNodes(w http.ResponseWriter, r *http.Request) {
	state, err := gm.NewNodeState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, toNodeResponses(state.Nodes))
}

func apiRefreshAllNodes(w http.ResponseWriter, r *http.Request) {
	state, err := gm.NewNodeState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	for i := range state.Nodes {
		refreshNodeFromProvider(ctx, &state.Nodes[i])
		_ = state.Update(state.Nodes[i].ID, func(n *gm.NodeRecord) {
			n.Status = state.Nodes[i].Status
			n.PublicIP = state.Nodes[i].PublicIP
		})
	}

	writeJSON(w, toNodeResponses(state.Nodes))
}

func apiRefreshNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, node := resolveNodeAPI(w, id)
	if node == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	refreshNodeFromProvider(ctx, node)
	_ = state.Update(node.ID, func(n *gm.NodeRecord) {
		n.Status = node.Status
		n.PublicIP = node.PublicIP
	})

	w.WriteHeader(http.StatusNoContent)
}

func apiStopNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, node := resolveNodeAPI(w, id)
	if node == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	provider, err := providerForNodeAPI(node)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := provider.StopInstance(ctx, node.ID); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	_ = state.Update(node.ID, func(n *gm.NodeRecord) { n.Status = "stopped" })
	w.WriteHeader(http.StatusNoContent)
}

func apiStartNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, node := resolveNodeAPI(w, id)
	if node == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	provider, err := providerForNodeAPI(node)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	inst, err := provider.StartInstance(ctx, node.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	_ = state.Update(node.ID, func(n *gm.NodeRecord) {
		n.Status = "running"
		n.PublicIP = inst.PublicIP
	})

	// Auto-deploy/restart agent in background.
	nodeCopy := *node
	nodeCopy.PublicIP = inst.PublicIP
	go func() {
		invalidateAgentCache(nodeCopy.ID)
		_ = deployAgentToNode(&nodeCopy)
	}()

	w.WriteHeader(http.StatusNoContent)
}

func apiDeployAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, node := resolveNodeAPI(w, id)
	if node == nil {
		return
	}

	if node.Status != "running" || node.PublicIP == "" {
		httpError(w, http.StatusBadRequest, "node must be running with a public IP")
		return
	}

	// Run deploy in background so we don't block the HTTP response.
	nodeCopy := *node
	go func() {
		log.Printf("agent-deploy: manual deploy triggered for node %s (%s)", nodeCopy.ID, nodeCopy.PublicIP)
		invalidateAgentCache(nodeCopy.ID)
		if err := deployAgentToNode(&nodeCopy); err != nil {
			log.Printf("agent-deploy: FAILED for node %s: %v", nodeCopy.ID, err)
		} else {
			log.Printf("agent-deploy: SUCCESS for node %s", nodeCopy.ID)
			invalidateAgentCache(nodeCopy.ID)
		}
	}()

	writeJSON(w, map[string]string{"status": "deploying"})
}

func apiDestroyNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	state, node := resolveNodeAPI(w, id)
	if node == nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	provider, err := providerForNodeAPI(node)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Check if this is the last node (to decide whether to clean up shared SG).
	isLastNode := len(state.Nodes) <= 1
	sgID := node.SecurityGrp

	_ = provider.Terminate(ctx, node.ID)
	state.RemoveKey(node.ID)
	_ = state.Remove(node.ID)

	// Delete shared security group if no more nodes remain.
	if isLastNode && sgID != "" {
		if ec2p, ok := provider.(*gm.EC2Provider); ok {
			_ = ec2p.DeleteSecurityGroup(ctx, node.ID)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

type createNodeRequest struct {
	Provider     string `json:"provider"`
	InstanceType string `json:"instance_type"`
	Region       string `json:"region"`
	Name         string `json:"name"`
	Account      string `json:"account"`
}

func apiCreateNode(w http.ResponseWriter, r *http.Request) {
	var req createNodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Provider == "" {
		req.Provider = "aws"
	}

	switch req.Provider {
	case "aws":
		apiCreateAWS(w, r, req)
	default:
		httpError(w, http.StatusBadRequest, "unsupported provider: "+req.Provider)
	}
}

func apiCreateAWS(w http.ResponseWriter, r *http.Request, req createNodeRequest) {
	config := resolveAWSConfig(req.Account, req.InstanceType, req.Region, "", "")

	if req.Name != "" {
		if config.Tags == nil {
			config.Tags = make(map[string]string)
		}
		config.Tags["Name"] = req.Name
	}

	provider := gm.NewEC2Provider(config)
	machine := gm.NewCloudMachine(provider)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	if err := machine.Start(ctx); err != nil {
		httpError(w, http.StatusInternalServerError, "launch failed: "+err.Error())
		return
	}

	// Resolve final display values.
	instanceType := req.InstanceType
	if instanceType == "" {
		instanceType = "t3.medium"
	}
	region := req.Region
	if region == "" {
		region = config.Region
		if region == "" {
			if envRegion := os.Getenv("AWS_REGION"); envRegion != "" {
				region = envRegion
			} else {
				region = "us-east-1"
			}
		}
	}

	// Save SSH key and state.
	state, err := gm.NewNodeState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "node created but failed to save state: "+err.Error())
		return
	}

	sshUser, privateKeyPEM := provider.SSHConfig()
	keyPath, _ := state.SaveKey(machine.ID(), privateKeyPEM)

	nodeName := req.Name
	if nodeName == "" {
		nodeName = "gitmachine"
	}

	record := gm.NodeRecord{
		ID:           machine.ID(),
		Provider:     "aws",
		InstanceType: instanceType,
		Region:       region,
		PublicIP:     machine.GetPublicIP(),
		SSHUser:      sshUser,
		SSHKeyPath:   keyPath,
		Status:       "running",
		CreatedAt:    time.Now(),
		Tags:         map[string]string{"Name": nodeName},
	}

	// Store AWS resource names for cleanup on destroy.
	record.SSHKeyName = provider.CreatedKeyName()
	record.SecurityGrp = provider.CreatedSGID()

	_ = state.Add(record)

	// Auto-deploy agent in background (don't block the response).
	go func() {
		log.Printf("agent-deploy: starting for node %s (%s)", record.ID, record.PublicIP)
		if err := deployAgentToNode(&record); err != nil {
			log.Printf("agent-deploy: FAILED for node %s: %v", record.ID, err)
		} else {
			log.Printf("agent-deploy: SUCCESS for node %s", record.ID)
			invalidateAgentCache(record.ID)
		}
	}()

	writeJSON(w, toNodeResponses([]gm.NodeRecord{record}))
}

func apiListCredentials(w http.ResponseWriter, r *http.Request) {
	store, err := gm.NewCredentialStore()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]credentialResponse, len(store.Credentials))
	for i, c := range store.Credentials {
		out[i] = credentialResponse{
			Name:     c.Name,
			Provider: c.Provider,
			Region:   c.Fields["region"],
			Default:  c.Default,
		}
	}
	writeJSON(w, out)
}

// --- sandbox API handlers ---

func apiListSandboxes(w http.ResponseWriter, r *http.Request) {
	sstate, err := gm.NewSandboxState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Sync: pull sandbox list from each running node's agent and add any
	// sandboxes that are missing from local state.
	nstate, _ := gm.NewNodeState()
	if nstate != nil {
		for i := range nstate.Nodes {
			node := &nstate.Nodes[i]
			if node.Status != "running" || !isAgentAvailable(node) {
				continue
			}
			remotes, err := agentListSandboxes(node)
			if err != nil {
				continue
			}
			for _, remote := range remotes {
				existing := sstate.Find(remote.ID)
				if existing == nil {
					existing = sstate.FindByPrefix(remote.ID)
				}
				if existing == nil {
					// Not tracked locally — add it.
					var ports []gm.PortMapping
					for _, p := range remote.Ports {
						ports = append(ports, gm.PortMapping{
							ContainerPort: p.ContainerPort,
							HostPort:      p.HostPort,
							URL:           p.URL,
						})
					}
					_ = sstate.Add(gm.SandboxRecord{
						ID:        remote.ID,
						Name:      remote.Name,
						NodeID:    node.ID,
						Image:     remote.Image,
						Status:    remote.Status,
						Ports:     ports,
						CreatedAt: time.Now(),
					})
				} else {
					// Update status and ports from agent.
					_ = sstate.Update(existing.ID, func(rec *gm.SandboxRecord) {
						rec.Status = remote.Status
						if len(rec.Ports) == 0 && len(remote.Ports) > 0 {
							var ports []gm.PortMapping
							for _, p := range remote.Ports {
								ports = append(ports, gm.PortMapping{
									ContainerPort: p.ContainerPort,
									HostPort:      p.HostPort,
									URL:           p.URL,
								})
							}
							rec.Ports = ports
						}
					})
				}
			}
		}
	}

	writeJSON(w, toSandboxResponses(sstate.Sandboxes))
}

func apiGetSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, rec := resolveSandboxAPI(w, id)
	if rec == nil {
		return
	}
	resp := toSandboxResponses([]gm.SandboxRecord{*rec})
	writeJSON(w, resp[0])
}

type execSandboxRequest struct {
	Cmd     string            `json:"cmd"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

type execSandboxResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func apiExecSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, rec := resolveSandboxAPI(w, id)
	if rec == nil {
		return
	}

	var req execSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Cmd == "" {
		httpError(w, http.StatusBadRequest, "cmd is required")
		return
	}

	node := resolveNodeForSandboxAPI(w, rec.NodeID)
	if node == nil {
		return
	}

	if !isAgentAvailable(node) {
		httpError(w, http.StatusServiceUnavailable, "agent not available on node "+node.ID)
		return
	}

	result, err := agentExecInSandbox(node, rec.ID, req)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "agent exec: "+err.Error())
		return
	}
	writeJSON(w, execSandboxResponse{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	})
}

type createSandboxRequest struct {
	NodeID   string `json:"node_id"`
	Image    string `json:"image"`
	Name     string `json:"name"`
	CPUs     string `json:"cpus"`
	Memory   string `json:"memory"`
	DiskSize string `json:"disk_size"`
	Ports    []int  `json:"ports"`
}

func apiCreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req createSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Resolve node — auto-select if node_id is empty.
	nstate, err := gm.NewNodeState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var node *gm.NodeRecord
	if req.NodeID != "" {
		node = nstate.Find(req.NodeID)
		if node == nil {
			node = findByPrefix(nstate, req.NodeID)
		}
		if node == nil {
			httpError(w, http.StatusNotFound, "node not found: "+req.NodeID)
			return
		}
	} else {
		// Auto-select: pick the running node with the fewest sandboxes.
		node = apiAutoSelectNode(nstate)
		if node == nil {
			httpError(w, http.StatusBadRequest, "no running nodes available")
			return
		}
	}

	if !isAgentAvailable(node) {
		httpError(w, http.StatusServiceUnavailable, "agent not available on node "+node.ID)
		return
	}

	info, err := agentCreateSandbox(node, req)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "agent create: "+err.Error())
		return
	}

	finalImage := req.Image
	if finalImage == "" {
		finalImage = "ubuntu:22.04"
	}

	sstate, err := gm.NewSandboxState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "save sandbox state: "+err.Error())
		return
	}

	var ports []gm.PortMapping
	for _, p := range info.Ports {
		ports = append(ports, gm.PortMapping{
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			URL:           p.URL,
		})
	}

	rec := gm.SandboxRecord{
		ID:        info.ID,
		Name:      info.Name,
		NodeID:    node.ID,
		Image:     finalImage,
		CPUs:      req.CPUs,
		Memory:    req.Memory,
		DiskSize:  req.DiskSize,
		Ports:     ports,
		Status:    "running",
		CreatedAt: time.Now(),
	}
	_ = sstate.Add(rec)

	writeJSON(w, toSandboxResponses([]gm.SandboxRecord{rec}))
}

func apiStartSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sstate, rec := resolveSandboxAPI(w, id)
	if rec == nil {
		return
	}

	node := resolveNodeForSandboxAPI(w, rec.NodeID)
	if node == nil {
		return
	}

	if !isAgentAvailable(node) {
		httpError(w, http.StatusServiceUnavailable, "agent not available on node "+node.ID)
		return
	}

	if err := agentStartSandbox(node, rec.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "agent start: "+err.Error())
		return
	}

	_ = sstate.Update(rec.ID, func(r *gm.SandboxRecord) { r.Status = "running" })
	w.WriteHeader(http.StatusNoContent)
}

func apiStopSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sstate, rec := resolveSandboxAPI(w, id)
	if rec == nil {
		return
	}

	node := resolveNodeForSandboxAPI(w, rec.NodeID)
	if node == nil {
		return
	}

	if !isAgentAvailable(node) {
		httpError(w, http.StatusServiceUnavailable, "agent not available on node "+node.ID)
		return
	}

	if err := agentStopSandbox(node, rec.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "agent stop: "+err.Error())
		return
	}

	_ = sstate.Update(rec.ID, func(r *gm.SandboxRecord) { r.Status = "stopped" })
	w.WriteHeader(http.StatusNoContent)
}

func apiDestroySandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sstate, rec := resolveSandboxAPI(w, id)
	if rec == nil {
		return
	}

	node := resolveNodeForSandboxAPI(w, rec.NodeID)
	if node == nil {
		// Node gone, just remove the record.
		_ = sstate.Remove(rec.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if isAgentAvailable(node) {
		_ = agentRemoveSandbox(node, rec.ID)
	}

	_ = sstate.Remove(rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

func apiSandboxSSHWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sstate, err := gm.NewSandboxState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rec := sstate.Find(id)
	if rec == nil {
		rec = sstate.FindByPrefix(id)
	}
	if rec == nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	// Find the parent node.
	log.Printf("sandbox-ssh: lookup node for sandbox %s (nodeID=%s)", rec.ID, rec.NodeID)
	nstate, err := gm.NewNodeState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	node := nstate.Find(rec.NodeID)
	if node == nil {
		http.Error(w, "parent node not found", http.StatusNotFound)
		return
	}
	if node.PublicIP == "" {
		http.Error(w, "node has no public IP", http.StatusBadRequest)
		return
	}

	// Read the private key.
	keyData, err := os.ReadFile(node.SSHKeyPath)
	if err != nil {
		http.Error(w, "cannot read SSH key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		http.Error(w, "invalid SSH key: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sshUser := node.SSHUser
	if sshUser == "" {
		sshUser = "ubuntu"
	}

	sshCfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         10 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", net.JoinHostPort(node.PublicIP, "22"), sshCfg)
	if err != nil {
		http.Error(w, "SSH connect failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	session, err := sshClient.NewSession()
	if err != nil {
		sshClient.Close()
		http.Error(w, "SSH session failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "PTY request failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "stdin pipe failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "stdout pipe failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Start a shell running docker exec into the container.
	dockerExecCmd := fmt.Sprintf("sudo docker exec -it %s /bin/bash || sudo docker exec -it %s /bin/sh", rec.ID, rec.ID)
	log.Printf("sandbox-ssh: starting docker exec for %s on %s", rec.ID, node.PublicIP)
	if err := session.Start(dockerExecCmd); err != nil {
		log.Printf("sandbox-ssh: docker exec start failed: %v", err)
		session.Close()
		sshClient.Close()
		http.Error(w, "docker exec failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Upgrade to WebSocket.
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		session.Close()
		sshClient.Close()
		return
	}

	// SSH stdout -> WebSocket.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.TextMessage, buf[:n]); writeErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		conn.Close()
	}()

	// WebSocket -> SSH stdin.
	go func() {
		defer func() {
			stdinPipe.Close()
			session.Close()
			sshClient.Close()
			conn.Close()
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if _, err := stdinPipe.Write(msg); err != nil {
				break
			}
		}
	}()
}

func resolveSandboxAPI(w http.ResponseWriter, idOrPrefix string) (*gm.SandboxState, *gm.SandboxRecord) {
	sstate, err := gm.NewSandboxState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil, nil
	}
	rec := sstate.Find(idOrPrefix)
	if rec == nil {
		rec = sstate.FindByPrefix(idOrPrefix)
	}
	if rec == nil {
		httpError(w, http.StatusNotFound, "sandbox not found: "+idOrPrefix)
		return nil, nil
	}
	return sstate, rec
}

// apiAutoSelectNode picks the running node with the fewest sandboxes.
func apiAutoSelectNode(nstate *gm.NodeState) *gm.NodeRecord {
	var running []int
	for i := range nstate.Nodes {
		if nstate.Nodes[i].Status == "running" && nstate.Nodes[i].PublicIP != "" {
			running = append(running, i)
		}
	}
	if len(running) == 0 {
		return nil
	}

	sstate, _ := gm.NewSandboxState()
	sandboxCount := make(map[string]int)
	if sstate != nil {
		for _, s := range sstate.Sandboxes {
			if s.Status == "running" {
				sandboxCount[s.NodeID]++
			}
		}
	}

	best := running[0]
	for _, idx := range running[1:] {
		if sandboxCount[nstate.Nodes[idx].ID] < sandboxCount[nstate.Nodes[best].ID] {
			best = idx
		}
	}
	return &nstate.Nodes[best]
}

func resolveNodeForSandboxAPI(w http.ResponseWriter, nodeID string) *gm.NodeRecord {
	nstate, err := gm.NewNodeState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil
	}
	node := nstate.Find(nodeID)
	if node == nil {
		httpError(w, http.StatusNotFound, "parent node not found: "+nodeID)
		return nil
	}
	return node
}

func toSandboxResponses(sandboxes []gm.SandboxRecord) []sandboxResponse {
	out := make([]sandboxResponse, len(sandboxes))
	for i, s := range sandboxes {
		sid := s.ID
		if len(sid) > 12 {
			sid = sid[:12]
		}
		nid := s.NodeID
		if len(nid) > 12 {
			nid = nid[:12]
		}
		// Check sandbox daemon status by probing port 9421.
		daemonStatus := "unknown"
		if s.Status == "running" {
			daemonStatus = "unreachable"
			for _, p := range s.Ports {
				if p.ContainerPort == 9421 && p.URL != "" {
					client := &http.Client{Timeout: 2 * time.Second}
					if resp, err := client.Get(p.URL + "/health"); err == nil {
						resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							daemonStatus = "running"
						}
					}
					break
				}
			}
		} else {
			daemonStatus = "stopped"
		}

		out[i] = sandboxResponse{
			ID:           s.ID,
			ShortID:      sid,
			Name:         s.Name,
			NodeID:       s.NodeID,
			NodeShort:    nid,
			Image:        s.Image,
			CPUs:         s.CPUs,
			Memory:       s.Memory,
			DiskSize:     s.DiskSize,
			Ports:        s.Ports,
			Status:       s.Status,
			DaemonStatus: daemonStatus,
			Age:          formatAge(time.Since(s.CreatedAt)),
			CreatedAt:    s.CreatedAt.Format(time.RFC3339),
		}
	}
	return out
}

// --- helpers ---

func resolveNodeAPI(w http.ResponseWriter, idOrPrefix string) (*gm.NodeState, *gm.NodeRecord) {
	state, err := gm.NewNodeState()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return nil, nil
	}

	node := state.Find(idOrPrefix)
	if node == nil {
		node = findByPrefix(state, idOrPrefix)
	}
	if node == nil {
		httpError(w, http.StatusNotFound, "node not found: "+idOrPrefix)
		return nil, nil
	}
	return state, node
}

func providerForNodeAPI(node *gm.NodeRecord) (gm.CloudProvider, error) {
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
		return p, nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", node.Provider)
	}
}

func refreshNodeFromProvider(ctx context.Context, node *gm.NodeRecord) {
	provider, err := providerForNodeAPI(node)
	if err != nil {
		node.Status = "unknown"
		return
	}
	inst, err := provider.Describe(ctx, node.ID)
	if err != nil {
		node.Status = "unknown"
		return
	}
	node.PublicIP = inst.PublicIP
	if inst.PublicIP != "" {
		node.Status = "running"
	} else {
		node.Status = "stopped"
	}
}

func toNodeResponses(nodes []gm.NodeRecord) []nodeResponse {
	out := make([]nodeResponse, len(nodes))
	for i, n := range nodes {
		name := n.Tags["Name"]
		if name == "" {
			name = "gitmachine"
		}

		agentStatus := "unknown"
		if n.Status == "running" && n.PublicIP != "" {
			if isAgentAvailable(&n) {
				agentStatus = "running"
			} else {
				agentStatus = "unreachable"
			}
		} else if n.Status == "stopped" {
			agentStatus = "stopped"
		}

		out[i] = nodeResponse{
			ID:           n.ID,
			Name:         name,
			Provider:     n.Provider,
			InstanceType: n.InstanceType,
			Region:       n.Region,
			PublicIP:     n.PublicIP,
			Status:       n.Status,
			AgentStatus:  agentStatus,
			Age:          formatAge(time.Since(n.CreatedAt)),
			CreatedAt:    n.CreatedAt.Format(time.RFC3339),
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	http.Error(w, strings.TrimSpace(msg), code)
}

// --- WebSocket SSH terminal ---

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func apiSSHWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	state, err := gm.NewNodeState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	node := state.Find(id)
	if node == nil {
		node = findByPrefix(state, id)
	}
	if node == nil {
		http.Error(w, "node not found", http.StatusNotFound)
		return
	}
	if node.PublicIP == "" {
		http.Error(w, "node has no public IP", http.StatusBadRequest)
		return
	}

	// Read the private key.
	keyData, err := os.ReadFile(node.SSHKeyPath)
	if err != nil {
		http.Error(w, "cannot read SSH key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		http.Error(w, "invalid SSH key: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sshUser := node.SSHUser
	if sshUser == "" {
		sshUser = "ubuntu"
	}

	sshCfg := &ssh.ClientConfig{
		User:            sshUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         10 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", net.JoinHostPort(node.PublicIP, "22"), sshCfg)
	if err != nil {
		http.Error(w, "SSH connect failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	session, err := sshClient.NewSession()
	if err != nil {
		sshClient.Close()
		http.Error(w, "SSH session failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Request PTY.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "PTY request failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	stdinPipe, err := session.StdinPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "stdin pipe failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "stdout pipe failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	if err := session.Shell(); err != nil {
		session.Close()
		sshClient.Close()
		http.Error(w, "shell start failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Upgrade to WebSocket.
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		session.Close()
		sshClient.Close()
		return
	}

	// SSH stdout -> WebSocket.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPipe.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.TextMessage, buf[:n]); writeErr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		conn.Close()
	}()

	// WebSocket -> SSH stdin.
	go func() {
		defer func() {
			stdinPipe.Close()
			session.Close()
			sshClient.Close()
			conn.Close()
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				break
			}
			if _, err := stdinPipe.Write(msg); err != nil {
				break
			}
		}
	}()
}
