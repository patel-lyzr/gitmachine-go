package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
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
	Age          string `json:"age"`
	CreatedAt    string `json:"created_at"`
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

	if err := http.ListenAndServe(":"+port, mux); err != nil {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
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
	w.WriteHeader(http.StatusNoContent)
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

	_ = provider.Terminate(ctx, node.ID)
	state.RemoveKey(node.ID)
	_ = state.Remove(node.ID)
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
	_ = state.Add(record)

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
		return gm.NewEC2Provider(config), nil
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
		out[i] = nodeResponse{
			ID:           n.ID,
			Name:         name,
			Provider:     n.Provider,
			InstanceType: n.InstanceType,
			Region:       n.Region,
			PublicIP:     n.PublicIP,
			Status:       n.Status,
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
