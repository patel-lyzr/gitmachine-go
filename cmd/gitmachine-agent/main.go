// gitmachine-agent is a lightweight daemon that runs on each EC2 node.
// It exposes a simple HTTP API for managing Docker containers, eliminating
// the SSH-per-request overhead and enabling sub-200ms sandbox operations.
//
// Endpoints:
//   POST   /sandbox          — create a new sandbox (docker run)
//   GET    /sandbox           — list sandboxes (docker ps)
//   GET    /sandbox/:id       — inspect a sandbox
//   POST   /sandbox/:id/exec  — execute a command in a sandbox
//   POST   /sandbox/:id/stop  — stop a sandbox
//   DELETE /sandbox/:id       — remove a sandbox
//   GET    /health            — health check
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultPort = 9420
const sandboxDaemonBinary = "/usr/local/bin/gitmachine-sandbox-daemon"
const sandboxDaemonPort = 9421

// ---------- request / response types ----------

type createRequest struct {
	Image    string `json:"image"`
	Name     string `json:"name"`
	CPUs     string `json:"cpus"`
	Memory   string `json:"memory"`
	DiskSize string `json:"disk_size"`
	Ports    []int  `json:"ports"`
}

type sandboxInfo struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Image  string        `json:"image"`
	Status string        `json:"status"`
	Ports  []portMapping `json:"ports,omitempty"`
}

type portMapping struct {
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	URL           string `json:"url,omitempty"`
}

type execRequest struct {
	Cmd     string            `json:"cmd"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// defaultSandboxPorts mirrors the defaults in docker_machine.go.
var defaultSandboxPorts = []int{22, 80, 443, 3000, 5000, 8000, 8080, 8888}

// ---------- global state ----------

var (
	useSudo  bool
	hostIP   string
	mu       sync.RWMutex
	// Track port mappings for sandboxes created by this agent.
	sandboxPorts = make(map[string][]portMapping) // containerID -> ports
)

func main() {
	port := defaultPort
	hostIP = detectHostIP()
	useSudo = detectSudo()

	// Recover port mappings from any containers already running on this node.
	recoverExistingSandboxes()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /sandbox", handleCreate)
	mux.HandleFunc("GET /sandbox", handleList)
	mux.HandleFunc("GET /sandbox/{id}", handleInspect)
	mux.HandleFunc("POST /sandbox/{id}/exec", handleExec)
	mux.HandleFunc("POST /sandbox/{id}/start", handleStart)
	mux.HandleFunc("POST /sandbox/{id}/stop", handleStop)
	mux.HandleFunc("DELETE /sandbox/{id}", handleRemove)

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	log.Printf("gitmachine-agent listening on %s (sudo=%v, hostIP=%s)", addr, useSudo, hostIP)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// ---------- handlers ----------

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}

	image := req.Image
	if image == "" {
		image = "ubuntu:22.04"
	}
	name := req.Name
	if name == "" {
		name = generateName()
	}

	// Pull image.
	if out, err := dockerRun("pull", image); err != nil {
		httpError(w, http.StatusInternalServerError, "pull: "+out)
		return
	}

	// Resolve ports — always include daemon port.
	ports := req.Ports
	if len(ports) == 0 {
		ports = defaultSandboxPorts
	}
	hasDaemonPort := false
	for _, p := range ports {
		if p == sandboxDaemonPort {
			hasDaemonPort = true
			break
		}
	}
	if !hasDaemonPort {
		ports = append(ports, sandboxDaemonPort)
	}
	usedPorts := getUsedHostPorts()
	nextPort := 10000
	var mappings []portMapping
	for _, cp := range ports {
		for usedPorts[nextPort] {
			nextPort++
		}
		hp := nextPort
		usedPorts[hp] = true
		nextPort++
		mappings = append(mappings, portMapping{
			ContainerPort: cp,
			HostPort:      hp,
			URL:           fmt.Sprintf("http://%s:%d", hostIP, hp),
		})
	}

	// Build docker run command.
	args := []string{"run", "-d", "--name", name, "--hostname", name}
	for _, pm := range mappings {
		args = append(args, "-p", fmt.Sprintf("%d:%d", pm.HostPort, pm.ContainerPort))
	}
	if req.CPUs != "" {
		args = append(args, "--cpus", req.CPUs)
	}
	if req.Memory != "" {
		args = append(args, "--memory", req.Memory)
	}
	if req.DiskSize != "" {
		args = append(args, "--storage-opt", "size="+req.DiskSize)
	}
	args = append(args, image, "sleep", "infinity")

	out, err := dockerRun(args...)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "docker run: "+out)
		return
	}

	containerID := strings.TrimSpace(out)

	mu.Lock()
	sandboxPorts[containerID] = mappings
	mu.Unlock()

	// Inject and start the sandbox daemon inside the container.
	go injectSandboxDaemon(containerID)

	shortID := containerID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	writeJSON(w, sandboxInfo{
		ID:     shortID,
		Name:   name,
		Image:  image,
		Status: "running",
		Ports:  mappings,
	})
}

func handleList(w http.ResponseWriter, r *http.Request) {
	out, err := dockerRun("ps", "-a", "--no-trunc", "--format", "{{.ID}}|{{.Names}}|{{.Image}}|{{.Status}}")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "docker ps: "+out)
		return
	}

	var sandboxes []sandboxInfo
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		status := "running"
		if strings.HasPrefix(parts[3], "Exited") {
			status = "stopped"
		} else if strings.HasPrefix(parts[3], "Paused") {
			status = "paused"
		}

		fullID := parts[0]
		shortID := fullID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}

		mu.RLock()
		ports := sandboxPorts[fullID]
		mu.RUnlock()

		sandboxes = append(sandboxes, sandboxInfo{
			ID:     shortID,
			Name:   parts[1],
			Image:  parts[2],
			Status: status,
			Ports:  ports,
		})
	}
	writeJSON(w, sandboxes)
}

func handleInspect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	out, err := dockerRun("inspect", "--format", "{{.Id}}|{{.Name}}|{{.Config.Image}}|{{.State.Status}}", id)
	if err != nil {
		httpError(w, http.StatusNotFound, "not found: "+out)
		return
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 4)
	if len(parts) < 4 {
		httpError(w, http.StatusInternalServerError, "unexpected inspect output")
		return
	}

	mu.RLock()
	ports := sandboxPorts[parts[0]]
	mu.RUnlock()

	writeJSON(w, sandboxInfo{
		ID:     parts[0],
		Name:   strings.TrimPrefix(parts[1], "/"),
		Image:  parts[2],
		Status: parts[3],
		Ports:  ports,
	})
}

func handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Cmd == "" {
		httpError(w, http.StatusBadRequest, "cmd required")
		return
	}

	args := []string{"exec"}
	for k, v := range req.Env {
		args = append(args, "-e", k+"="+v)
	}
	if req.Cwd != "" {
		args = append(args, "-w", req.Cwd)
	}
	args = append(args, id, "sh", "-c", req.Cmd)

	timeout := 10 * time.Minute
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}

	stdout, stderr, exitCode, err := dockerRunFull(timeout, args...)
	if err != nil && exitCode == -1 {
		httpError(w, http.StatusInternalServerError, "exec: "+err.Error())
		return
	}

	writeJSON(w, execResponse{
		ExitCode: exitCode,
		Stdout:   stdout,
		Stderr:   stderr,
	})
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if out, err := dockerRun("start", id); err != nil {
		httpError(w, http.StatusInternalServerError, "start: "+out)
		return
	}

	// Re-inject the sandbox daemon since it won't survive a container stop/start.
	fullID := id
	if out, err := dockerRun("inspect", "--format", "{{.Id}}", id); err == nil {
		fullID = strings.TrimSpace(out)
	}
	go injectSandboxDaemon(fullID)

	w.WriteHeader(http.StatusNoContent)
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if out, err := dockerRun("stop", id); err != nil {
		httpError(w, http.StatusInternalServerError, "stop: "+out)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Get full ID before removing for port cleanup.
	fullID := id
	if out, err := dockerRun("inspect", "--format", "{{.Id}}", id); err == nil {
		fullID = strings.TrimSpace(out)
	}

	if out, err := dockerRun("rm", "-f", id); err != nil {
		httpError(w, http.StatusInternalServerError, "rm: "+out)
		return
	}

	mu.Lock()
	delete(sandboxPorts, fullID)
	mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
}

// ---------- sandbox daemon injection ----------

// injectSandboxDaemon copies the sandbox daemon binary into a running container
// and starts it in the background. This provides rich in-container APIs for
// file operations, git, process sessions, etc.
func injectSandboxDaemon(containerID string) {
	// Check if daemon binary exists on host.
	if _, err := os.Stat(sandboxDaemonBinary); err != nil {
		log.Printf("sandbox-daemon: binary not found at %s, skipping injection", sandboxDaemonBinary)
		return
	}

	// docker cp the binary into the container.
	cpArgs := []string{"cp", sandboxDaemonBinary, containerID + ":/usr/local/bin/gitmachine-sandbox-daemon"}
	if out, err := dockerRun(cpArgs...); err != nil {
		log.Printf("sandbox-daemon: copy failed for %s: %s", containerID[:12], out)
		return
	}

	// Make it executable.
	if out, err := dockerRun("exec", containerID, "chmod", "+x", "/usr/local/bin/gitmachine-sandbox-daemon"); err != nil {
		log.Printf("sandbox-daemon: chmod failed for %s: %s", containerID[:12], out)
		return
	}

	// Start the daemon in the background inside the container.
	if out, err := dockerRun("exec", "-d", containerID, "/usr/local/bin/gitmachine-sandbox-daemon"); err != nil {
		log.Printf("sandbox-daemon: start failed for %s: %s", containerID[:12], out)
		return
	}

	log.Printf("sandbox-daemon: injected into %s (port %d)", containerID[:12], sandboxDaemonPort)
}

// ---------- docker helpers ----------

func dockerCmd(args ...string) *exec.Cmd {
	if useSudo {
		return exec.Command("sudo", append([]string{"docker"}, args...)...)
	}
	return exec.Command("docker", args...)
}

func dockerRun(args ...string) (string, error) {
	cmd := dockerCmd(args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func dockerRunFull(timeout time.Duration, args ...string) (stdout, stderr string, exitCode int, err error) {
	cmd := dockerCmd(args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return "", "", -1, err
	}
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		_ = cmd.Process.Kill()
		return stdoutBuf.String(), stderrBuf.String(), -1, fmt.Errorf("timeout after %s", timeout)
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				return stdoutBuf.String(), stderrBuf.String(), -1, err
			}
		}
		return stdoutBuf.String(), stderrBuf.String(), exitCode, nil
	}
}

func getUsedHostPorts() map[int]bool {
	used := make(map[int]bool)
	out, err := dockerRun("ps", "--format", "{{.Ports}}")
	if err != nil {
		return used
	}
	for _, line := range strings.Split(out, "\n") {
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			if idx := strings.Index(part, "->"); idx > 0 {
				hostPart := part[:idx]
				if ci := strings.LastIndex(hostPart, ":"); ci >= 0 {
					portStr := hostPart[ci+1:]
					if p, err := strconv.Atoi(portStr); err == nil {
						used[p] = true
					}
				}
			}
		}
	}
	return used
}

// ---------- recovery ----------

// recoverExistingSandboxes scans running Docker containers and rebuilds the
// in-memory sandboxPorts map so the agent survives restarts without losing
// track of existing sandboxes.
func recoverExistingSandboxes() {
	// Format: full container ID, then port bindings like "0.0.0.0:10000->8080/tcp, ..."
	out, err := dockerRun("ps", "-a", "--no-trunc", "--format", "{{.ID}}|{{.Ports}}")
	if err != nil {
		log.Printf("recovery: docker ps failed: %v", err)
		return
	}

	recovered := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		containerID := parts[0]
		var mappings []portMapping
		for _, chunk := range strings.Split(parts[1], ",") {
			chunk = strings.TrimSpace(chunk)
			// Skip IPv6 duplicates like "[::]:10000->22/tcp".
			if strings.HasPrefix(chunk, "[") {
				continue
			}
			// Parse "0.0.0.0:10000->8080/tcp"
			arrow := strings.Index(chunk, "->")
			if arrow < 0 {
				continue
			}
			hostPart := chunk[:arrow]
			contPart := chunk[arrow+2:]

			// Extract host port from "0.0.0.0:10000"
			ci := strings.LastIndex(hostPart, ":")
			if ci < 0 {
				continue
			}
			hp, err := strconv.Atoi(hostPart[ci+1:])
			if err != nil {
				continue
			}

			// Extract container port from "8080/tcp"
			cpStr := strings.Split(contPart, "/")[0]
			cp, err := strconv.Atoi(cpStr)
			if err != nil {
				continue
			}

			mappings = append(mappings, portMapping{
				ContainerPort: cp,
				HostPort:      hp,
				URL:           fmt.Sprintf("http://%s:%d", hostIP, hp),
			})
		}
		if len(mappings) > 0 {
			mu.Lock()
			sandboxPorts[containerID] = mappings
			mu.Unlock()
			recovered++
		}
	}
	if recovered > 0 {
		log.Printf("recovery: restored port mappings for %d containers", recovered)
	}
}

// ---------- utility ----------

func detectSudo() bool {
	cmd := exec.Command("docker", "version")
	if err := cmd.Run(); err != nil {
		return true
	}
	return false
}

func detectHostIP() string {
	// Try EC2 metadata service first (IMDSv2).
	cmd := exec.Command("curl", "-sf", "--connect-timeout", "1", "-H", "X-aws-ec2-metadata-token-ttl-seconds:21600", "-X", "PUT", "http://169.254.169.254/latest/api/token")
	tokenOut, err := cmd.Output()
	if err == nil {
		token := strings.TrimSpace(string(tokenOut))
		cmd2 := exec.Command("curl", "-sf", "--connect-timeout", "1", "-H", "X-aws-ec2-metadata-token:"+token, "http://169.254.169.254/latest/meta-data/public-ipv4")
		ipOut, err := cmd2.Output()
		if err == nil {
			ip := strings.TrimSpace(string(ipOut))
			if ip != "" {
				return ip
			}
		}
	}

	// Fallback: try hostname -I.
	cmd = exec.Command("hostname", "-I")
	out, err := cmd.Output()
	if err == nil {
		parts := strings.Fields(string(out))
		if len(parts) > 0 {
			return parts[0]
		}
	}

	return "localhost"
}

func generateName() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "gm-" + hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	http.Error(w, strings.TrimSpace(msg), code)
}
