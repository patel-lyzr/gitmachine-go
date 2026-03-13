package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	gm "github.com/open-gitagent/gitmachine-go"
)

const agentPort = 9420

// agentAvailability caches whether a node has a reachable agent.
var (
	agentCache   = make(map[string]bool) // nodeID -> agent reachable
	agentCacheMu sync.RWMutex
)

// agentURL returns the base URL for the agent on a node.
func agentURL(node *gm.NodeRecord) string {
	return fmt.Sprintf("http://%s:%d", node.PublicIP, agentPort)
}

// isAgentAvailable checks (with caching) whether the agent is running on a node.
func isAgentAvailable(node *gm.NodeRecord) bool {
	if node.PublicIP == "" {
		return false
	}

	agentCacheMu.RLock()
	cached, ok := agentCache[node.ID]
	agentCacheMu.RUnlock()
	if ok {
		return cached
	}

	// Probe the agent.
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(agentURL(node) + "/health")
	available := err == nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}

	// Only cache positive results — keep retrying if unreachable.
	if available {
		agentCacheMu.Lock()
		agentCache[node.ID] = true
		agentCacheMu.Unlock()
	}

	return available
}

// invalidateAgentCache removes a node from the agent availability cache.
func invalidateAgentCache(nodeID string) {
	agentCacheMu.Lock()
	delete(agentCache, nodeID)
	agentCacheMu.Unlock()
}

// --- Agent-based operations (bypass SSH) ---

type agentCreateRequest struct {
	Image    string `json:"image"`
	Name     string `json:"name"`
	CPUs     string `json:"cpus"`
	Memory   string `json:"memory"`
	DiskSize string `json:"disk_size"`
	Ports    []int  `json:"ports"`
}

type agentSandboxInfo struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Image  string          `json:"image"`
	Status string          `json:"status"`
	Ports  []gm.PortMapping `json:"ports,omitempty"`
}

type agentExecRequest struct {
	Cmd     string            `json:"cmd"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

type agentExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// agentCreateSandbox creates a sandbox via the agent HTTP API.
func agentCreateSandbox(node *gm.NodeRecord, req createSandboxRequest) (*agentSandboxInfo, error) {
	body := agentCreateRequest{
		Image:    req.Image,
		Name:     req.Name,
		CPUs:     req.CPUs,
		Memory:   req.Memory,
		DiskSize: req.DiskSize,
		Ports:    req.Ports,
	}

	data, _ := json.Marshal(body)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post(agentURL(node)+"/sandbox", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("agent create: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent create: %s", strings.TrimSpace(string(errBody)))
	}

	var info agentSandboxInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("agent decode: %w", err)
	}
	return &info, nil
}

// agentExecInSandbox executes a command via the agent HTTP API.
func agentExecInSandbox(node *gm.NodeRecord, sandboxID string, req execSandboxRequest) (*agentExecResponse, error) {
	body := agentExecRequest{
		Cmd:     req.Cmd,
		Cwd:     req.Cwd,
		Env:     req.Env,
		Timeout: req.Timeout,
	}

	data, _ := json.Marshal(body)
	timeout := 10 * time.Minute
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout+30) * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(agentURL(node)+"/sandbox/"+sandboxID+"/exec", "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("agent exec: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent exec: %s", strings.TrimSpace(string(errBody)))
	}

	var result agentExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("agent decode: %w", err)
	}
	return &result, nil
}

// agentListSandboxes lists sandboxes via the agent HTTP API.
func agentListSandboxes(node *gm.NodeRecord) ([]agentSandboxInfo, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(agentURL(node) + "/sandbox")
	if err != nil {
		return nil, fmt.Errorf("agent list: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agent list: %s", strings.TrimSpace(string(errBody)))
	}

	var infos []agentSandboxInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return nil, fmt.Errorf("agent decode: %w", err)
	}
	return infos, nil
}

// agentStartSandbox starts a stopped sandbox via the agent HTTP API.
func agentStartSandbox(node *gm.NodeRecord, sandboxID string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	req, _ := http.NewRequest("POST", agentURL(node)+"/sandbox/"+sandboxID+"/start", nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("agent start: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent start: status %d", resp.StatusCode)
	}
	return nil
}

// agentStopSandbox stops a sandbox via the agent HTTP API.
func agentStopSandbox(node *gm.NodeRecord, sandboxID string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	req, _ := http.NewRequest("POST", agentURL(node)+"/sandbox/"+sandboxID+"/stop", nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("agent stop: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent stop: status %d", resp.StatusCode)
	}
	return nil
}

// agentRemoveSandbox removes a sandbox via the agent HTTP API.
func agentRemoveSandbox(node *gm.NodeRecord, sandboxID string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	req, _ := http.NewRequest("DELETE", agentURL(node)+"/sandbox/"+sandboxID, nil)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("agent rm: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent rm: status %d", resp.StatusCode)
	}
	return nil
}

// --- Background agent health checker ---

const (
	healthCheckInterval = 30 * time.Second // How often to check agent health.
	unreachableTimeout  = 60 * time.Second // How long unreachable before auto-deploy.
)

var (
	// Tracks when each node was first seen as unreachable.
	unreachableSince   = make(map[string]time.Time)
	unreachableSinceMu sync.Mutex
	// Tracks nodes currently being deployed to (prevent concurrent deploys).
	deployingNodes   = make(map[string]bool)
	deployingNodesMu sync.Mutex
)

// agentHealthLoop periodically checks running nodes and auto-deploys the agent
// if it has been unreachable for longer than unreachableTimeout.
func agentHealthLoop() {
	// Wait for initial sync to finish before starting checks.
	time.Sleep(10 * time.Second)
	log.Println("health: background agent health checker started")

	for {
		checkAndDeployAgents()
		time.Sleep(healthCheckInterval)
	}
}

func checkAndDeployAgents() {
	state, err := gm.NewNodeState()
	if err != nil {
		return
	}

	for i := range state.Nodes {
		node := &state.Nodes[i]
		if node.Status != "running" || node.PublicIP == "" {
			// Node not running — clear any unreachable tracking.
			unreachableSinceMu.Lock()
			delete(unreachableSince, node.ID)
			unreachableSinceMu.Unlock()
			continue
		}

		reachable := isAgentAvailable(node)

		unreachableSinceMu.Lock()
		if reachable {
			// Agent is healthy — clear tracking.
			delete(unreachableSince, node.ID)
			unreachableSinceMu.Unlock()
			continue
		}

		// Agent is unreachable.
		firstSeen, tracked := unreachableSince[node.ID]
		if !tracked {
			unreachableSince[node.ID] = time.Now()
			unreachableSinceMu.Unlock()
			log.Printf("health: agent unreachable on %s, will auto-deploy in %s", node.ID, unreachableTimeout)
			continue
		}
		unreachableSinceMu.Unlock()

		// Check if we've waited long enough.
		if time.Since(firstSeen) < unreachableTimeout {
			continue
		}

		// Skip nodes without SSH keys — can't deploy without them.
		if node.SSHKeyPath == "" {
			log.Printf("health: skipping auto-deploy for %s — no SSH key available. Destroy and recreate, or manually place key at ~/.gitmachine/keys/%s.pem", node.ID, node.ID)
			// Clear tracking so we don't spam the log every 30s.
			unreachableSinceMu.Lock()
			delete(unreachableSince, node.ID)
			unreachableSinceMu.Unlock()
			continue
		}

		// Check if already deploying.
		deployingNodesMu.Lock()
		if deployingNodes[node.ID] {
			deployingNodesMu.Unlock()
			continue
		}
		deployingNodes[node.ID] = true
		deployingNodesMu.Unlock()

		// Auto-deploy in background.
		nodeCopy := *node
		go func() {
			defer func() {
				deployingNodesMu.Lock()
				delete(deployingNodes, nodeCopy.ID)
				deployingNodesMu.Unlock()
			}()

			log.Printf("health: auto-deploying agent to %s (%s) — unreachable for %s",
				nodeCopy.ID, nodeCopy.PublicIP, time.Since(firstSeen).Round(time.Second))
			invalidateAgentCache(nodeCopy.ID)

			if err := deployAgentToNode(&nodeCopy); err != nil {
				log.Printf("health: auto-deploy FAILED for %s: %v", nodeCopy.ID, err)
			} else {
				log.Printf("health: auto-deploy SUCCESS for %s", nodeCopy.ID)
				invalidateAgentCache(nodeCopy.ID)
				// Clear unreachable tracking on success.
				unreachableSinceMu.Lock()
				delete(unreachableSince, nodeCopy.ID)
				unreachableSinceMu.Unlock()
			}
		}()
	}
}
