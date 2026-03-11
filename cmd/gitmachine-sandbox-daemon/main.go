// gitmachine-sandbox-daemon runs INSIDE each sandbox container.
// It provides a rich HTTP API for file operations, git, process execution,
// PTY terminals, and more — eliminating the need for `docker exec` overhead.
//
// Endpoints:
//
//	GET    /health
//
//	# Files
//	GET    /files?path=              — list directory
//	GET    /files/read?path=         — read file contents
//	POST   /files/write              — write file
//	POST   /files/upload             — upload file (multipart)
//	GET    /files/download?path=     — download file
//	POST   /files/mkdir              — create directory
//	POST   /files/move               — move/rename
//	DELETE /files?path=              — delete file or directory
//	GET    /files/info?path=         — file info (size, perms, modtime)
//	GET    /files/search?path=&q=    — search file contents (grep)
//	POST   /files/chmod              — change file permissions
//
//	# Process
//	POST   /process/exec             — execute command (blocking)
//	POST   /process/session          — create persistent session
//	POST   /process/session/{id}/exec — run in session
//	GET    /process/session/{id}     — get session output
//	DELETE /process/session/{id}     — kill session
//
//	# PTY (interactive terminal via WebSocket)
//	POST   /process/pty              — create PTY session
//	GET    /process/pty              — list PTY sessions
//	GET    /process/pty/{id}         — get PTY session info
//	GET    /process/pty/{id}/connect — WebSocket attach
//	POST   /process/pty/{id}/resize  — resize terminal
//	DELETE /process/pty/{id}         — kill PTY session
//
//	# Git
//	POST   /git/clone                — clone repo
//	GET    /git/status?path=         — git status
//	POST   /git/commit               — stage & commit
//	POST   /git/push                 — push
//	POST   /git/pull                 — pull
//	GET    /git/branches?path=       — list branches
//	GET    /git/log?path=            — commit log
//
//	# Ports
//	GET    /ports                    — list ports in use
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const daemonPort = 9421

func main() {
	mux := http.NewServeMux()

	// Health
	mux.HandleFunc("GET /health", handleHealth)

	// Files
	mux.HandleFunc("GET /files", handleFileList)
	mux.HandleFunc("GET /files/read", handleFileRead)
	mux.HandleFunc("POST /files/write", handleFileWrite)
	mux.HandleFunc("POST /files/upload", handleFileUpload)
	mux.HandleFunc("GET /files/download", handleFileDownload)
	mux.HandleFunc("POST /files/mkdir", handleMkdir)
	mux.HandleFunc("POST /files/move", handleFileMove)
	mux.HandleFunc("DELETE /files", handleFileDelete)
	mux.HandleFunc("GET /files/info", handleFileInfo)
	mux.HandleFunc("GET /files/search", handleFileSearch)
	mux.HandleFunc("POST /files/chmod", handleFileChmod)

	// Process
	mux.HandleFunc("POST /process/exec", handleProcessExec)
	mux.HandleFunc("POST /process/session", handleSessionCreate)
	mux.HandleFunc("POST /process/session/{id}/exec", handleSessionExec)
	mux.HandleFunc("GET /process/session/{id}", handleSessionGet)
	mux.HandleFunc("DELETE /process/session/{id}", handleSessionDelete)

	// PTY (interactive terminal via WebSocket)
	mux.HandleFunc("POST /process/pty", handlePTYCreate)
	mux.HandleFunc("GET /process/pty", handlePTYList)
	mux.HandleFunc("GET /process/pty/{id}", handlePTYGet)
	mux.HandleFunc("GET /process/pty/{id}/connect", handlePTYConnect)
	mux.HandleFunc("POST /process/pty/{id}/resize", handlePTYResize)
	mux.HandleFunc("DELETE /process/pty/{id}", handlePTYDelete)

	// Git
	mux.HandleFunc("POST /git/clone", handleGitClone)
	mux.HandleFunc("GET /git/status", handleGitStatus)
	mux.HandleFunc("POST /git/commit", handleGitCommit)
	mux.HandleFunc("POST /git/push", handleGitPush)
	mux.HandleFunc("POST /git/pull", handleGitPull)
	mux.HandleFunc("GET /git/branches", handleGitBranches)
	mux.HandleFunc("GET /git/log", handleGitLog)

	// Ports
	mux.HandleFunc("GET /ports", handlePortList)
	mux.HandleFunc("GET /ports/check", handlePortCheck)

	addr := fmt.Sprintf("0.0.0.0:%d", daemonPort)
	log.Printf("gitmachine-sandbox-daemon listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// ── Health ──────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "component": "sandbox-daemon"})
}

// ── Files ───────────────────────────────────────────────────────────────────

type fileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
}

func handleFileList(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/"
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	var files []fileEntry
	for _, e := range entries {
		info, _ := e.Info()
		size := int64(0)
		mode := ""
		modTime := ""
		if info != nil {
			size = info.Size()
			mode = info.Mode().String()
			modTime = info.ModTime().Format(time.RFC3339)
		}
		files = append(files, fileEntry{
			Name:    e.Name(),
			Path:    filepath.Join(dir, e.Name()),
			IsDir:   e.IsDir(),
			Size:    size,
			Mode:    mode,
			ModTime: modTime,
		})
	}
	writeJSON(w, files)
}

func handleFileRead(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, map[string]string{"path": path, "content": string(data)})
}

func handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Mode    int    `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Path == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}

	mode := os.FileMode(0644)
	if req.Mode != 0 {
		mode = os.FileMode(req.Mode)
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(req.Path), 0755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := os.WriteFile(req.Path, []byte(req.Content), mode); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, map[string]string{"status": "ok", "path": req.Path})
}

func handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100MB max
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	destDir := r.FormValue("path")
	if destDir == "" {
		destDir = "/tmp"
	}

	var uploaded []string
	for _, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			src, err := fh.Open()
			if err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}

			destPath := filepath.Join(destDir, fh.Filename)
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				src.Close()
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}

			dst, err := os.Create(destPath)
			if err != nil {
				src.Close()
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}

			_, err = io.Copy(dst, src)
			src.Close()
			dst.Close()
			if err != nil {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
			uploaded = append(uploaded, destPath)
		}
	}

	writeJSON(w, map[string]interface{}{"status": "ok", "files": uploaded})
}

func handleFileDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if info.IsDir() {
		httpError(w, http.StatusBadRequest, "cannot download directory")
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	http.ServeFile(w, r, path)
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}

	if err := os.MkdirAll(req.Path, 0755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "path": req.Path})
}

func handleFileMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Source == "" || req.Destination == "" {
		httpError(w, http.StatusBadRequest, "source and destination required")
		return
	}

	if err := os.MkdirAll(filepath.Dir(req.Destination), 0755); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := os.Rename(req.Source, req.Destination); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func handleFileDelete(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}

	if err := os.RemoveAll(path); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleFileInfo(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, http.StatusBadRequest, "path required")
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, fileEntry{
		Name:    info.Name(),
		Path:    path,
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime().Format(time.RFC3339),
	})
}

func handleFileSearch(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	query := r.URL.Query().Get("q")
	if dir == "" || query == "" {
		httpError(w, http.StatusBadRequest, "path and q required")
		return
	}

	// Use grep for speed.
	cmd := exec.Command("grep", "-rn", "--include=*", "-l", query, dir)
	out, _ := cmd.Output()

	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			matches = append(matches, line)
		}
	}
	writeJSON(w, map[string]interface{}{"query": query, "matches": matches})
}

func handleFileChmod(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Mode int    `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" || req.Mode == 0 {
		httpError(w, http.StatusBadRequest, "path and mode required")
		return
	}
	if err := os.Chmod(req.Path, os.FileMode(req.Mode)); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "path": req.Path})
}

// ── Process ─────────────────────────────────────────────────────────────────

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

func handleProcessExec(w http.ResponseWriter, r *http.Request) {
	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		httpError(w, http.StatusBadRequest, "cmd required")
		return
	}

	cmd := exec.Command("sh", "-c", req.Cmd)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	timeout := 10 * time.Minute
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-timer.C:
		_ = cmd.Process.Kill()
		writeJSON(w, execResponse{ExitCode: -1, Stdout: stdout.String(), Stderr: "timeout"})
	case err := <-done:
		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				exitCode = exitErr.ExitCode()
			} else {
				httpError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		writeJSON(w, execResponse{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()})
	}
}

// ── Sessions (persistent shell) ─────────────────────────────────────────────

type session struct {
	ID        string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	output    []string
	mu        sync.Mutex
	createdAt time.Time
}

var (
	sessions   = make(map[string]*session)
	sessionsMu sync.RWMutex
)

func handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Shell string `json:"shell"`
		Cwd   string `json:"cwd"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	shell := req.Shell
	if shell == "" {
		shell = "/bin/sh"
		if _, err := os.Stat("/bin/bash"); err == nil {
			shell = "/bin/bash"
		}
	}

	id := generateID()
	cmd := exec.Command(shell)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = os.Environ()

	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	s := &session{
		ID:        id,
		cmd:       cmd,
		stdin:     stdin,
		createdAt: time.Now(),
	}

	if err := cmd.Start(); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Collect output in background.
	collectOutput := func(reader io.Reader) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB buffer
		for scanner.Scan() {
			s.mu.Lock()
			s.output = append(s.output, scanner.Text())
			// Keep last 10000 lines.
			if len(s.output) > 10000 {
				s.output = s.output[len(s.output)-10000:]
			}
			s.mu.Unlock()
		}
	}
	go collectOutput(stdout)
	go collectOutput(stderr)

	sessionsMu.Lock()
	sessions[id] = s
	sessionsMu.Unlock()

	writeJSON(w, map[string]string{"id": id, "shell": shell})
}

func handleSessionExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sessionsMu.RLock()
	s, ok := sessions[id]
	sessionsMu.RUnlock()
	if !ok {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}

	var req struct {
		Cmd string `json:"cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmd == "" {
		httpError(w, http.StatusBadRequest, "cmd required")
		return
	}

	// Record output length before command.
	s.mu.Lock()
	startIdx := len(s.output)
	s.mu.Unlock()

	// Send command to shell.
	_, err := fmt.Fprintln(s.stdin, req.Cmd)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Wait a bit for output.
	time.Sleep(200 * time.Millisecond)

	s.mu.Lock()
	newOutput := make([]string, len(s.output[startIdx:]))
	copy(newOutput, s.output[startIdx:])
	s.mu.Unlock()

	writeJSON(w, map[string]interface{}{"output": newOutput})
}

func handleSessionGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sessionsMu.RLock()
	s, ok := sessions[id]
	sessionsMu.RUnlock()
	if !ok {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}

	s.mu.Lock()
	output := make([]string, len(s.output))
	copy(output, s.output)
	s.mu.Unlock()

	running := s.cmd.ProcessState == nil
	writeJSON(w, map[string]interface{}{
		"id":         s.ID,
		"running":    running,
		"output":     output,
		"created_at": s.createdAt.Format(time.RFC3339),
	})
}

func handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	sessionsMu.Lock()
	s, ok := sessions[id]
	if ok {
		delete(sessions, id)
	}
	sessionsMu.Unlock()

	if !ok {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}

	s.stdin.Close()
	_ = s.cmd.Process.Kill()
	w.WriteHeader(http.StatusNoContent)
}

// ── PTY (interactive terminal via WebSocket) ────────────────────────────────

type ptySession struct {
	ID        string
	Shell     string
	Cwd       string
	cmd       *exec.Cmd
	ptmx      *os.File
	clients   map[string]*wsClient
	clientsMu sync.Mutex
	inCh      chan []byte
	doneCh    chan struct{}
	createdAt time.Time
	cols      uint16
	rows      uint16
}

type wsClient struct {
	id   string
	conn *websocket.Conn
	send chan []byte
}

var (
	ptySessions   = make(map[string]*ptySession)
	ptySessionsMu sync.RWMutex
	wsUpgrader    = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
)

func handlePTYCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Shell string `json:"shell"`
		Cwd   string `json:"cwd"`
		Cols  uint16 `json:"cols"`
		Rows  uint16 `json:"rows"`
		Env   map[string]string `json:"env"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	shell := req.Shell
	if shell == "" {
		shell = "/bin/sh"
		if _, err := os.Stat("/bin/bash"); err == nil {
			shell = "/bin/bash"
		}
	}
	cols := req.Cols
	if cols == 0 {
		cols = 80
	}
	rows := req.Rows
	if rows == 0 {
		rows = 24
	}

	id := generateID()
	cmd := exec.Command(shell)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
	}
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "start pty: "+err.Error())
		return
	}

	s := &ptySession{
		ID:        id,
		Shell:     shell,
		Cwd:       req.Cwd,
		cmd:       cmd,
		ptmx:      ptmx,
		clients:   make(map[string]*wsClient),
		inCh:      make(chan []byte, 256),
		doneCh:    make(chan struct{}),
		createdAt: time.Now(),
		cols:      cols,
		rows:      rows,
	}

	ptySessionsMu.Lock()
	ptySessions[id] = s
	ptySessionsMu.Unlock()

	// Read PTY output and broadcast to all clients.
	go s.readLoop()
	// Write client input to PTY.
	go s.writeLoop()
	// Wait for process to exit.
	go s.waitExit()

	log.Printf("pty: created session %s (shell=%s)", id, shell)
	writeJSON(w, map[string]interface{}{
		"id": id, "shell": shell, "cols": cols, "rows": rows,
	})
}

func (s *ptySession) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			s.broadcast(data)
		}
		if err != nil {
			return
		}
	}
}

func (s *ptySession) writeLoop() {
	for {
		select {
		case data := <-s.inCh:
			s.ptmx.Write(data)
		case <-s.doneCh:
			return
		}
	}
}

func (s *ptySession) waitExit() {
	err := s.cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	close(s.doneCh)
	s.ptmx.Close()

	// Close all WebSocket clients with exit code.
	s.clientsMu.Lock()
	for _, c := range s.clients {
		msg := websocket.FormatCloseMessage(
			websocket.CloseNormalClosure,
			fmt.Sprintf(`{"exitCode":%d}`, exitCode),
		)
		c.conn.WriteMessage(websocket.CloseMessage, msg)
		c.conn.Close()
	}
	s.clients = make(map[string]*wsClient)
	s.clientsMu.Unlock()

	log.Printf("pty: session %s exited (code=%d)", s.ID, exitCode)

	ptySessionsMu.Lock()
	delete(ptySessions, s.ID)
	ptySessionsMu.Unlock()
}

func (s *ptySession) broadcast(data []byte) {
	s.clientsMu.Lock()
	defer s.clientsMu.Unlock()
	for id, c := range s.clients {
		select {
		case c.send <- data:
		default:
			// Client too slow — disconnect.
			c.conn.Close()
			delete(s.clients, id)
		}
	}
}

func handlePTYConnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ptySessionsMu.RLock()
	s, ok := ptySessions[id]
	ptySessionsMu.RUnlock()
	if !ok {
		httpError(w, http.StatusNotFound, "pty session not found")
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	clientID := generateID()
	client := &wsClient{id: clientID, conn: conn, send: make(chan []byte, 256)}

	s.clientsMu.Lock()
	s.clients[clientID] = client
	s.clientsMu.Unlock()

	log.Printf("pty: client %s attached to session %s", clientID, id)

	// Send connected control message.
	conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"control","status":"connected"}`))

	// Writer: send PTY output to WebSocket.
	go func() {
		defer conn.Close()
		for data := range client.send {
			if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
				break
			}
		}
	}()

	// Reader: send WebSocket input to PTY.
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		select {
		case s.inCh <- msg:
		case <-s.doneCh:
			break
		}
	}

	s.clientsMu.Lock()
	delete(s.clients, clientID)
	s.clientsMu.Unlock()
	close(client.send)
	log.Printf("pty: client %s detached from session %s", clientID, id)
}

func handlePTYResize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ptySessionsMu.RLock()
	s, ok := ptySessions[id]
	ptySessionsMu.RUnlock()
	if !ok {
		httpError(w, http.StatusNotFound, "pty session not found")
		return
	}

	var req struct {
		Cols uint16 `json:"cols"`
		Rows uint16 `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cols == 0 || req.Rows == 0 {
		httpError(w, http.StatusBadRequest, "cols and rows required")
		return
	}

	if err := pty.Setsize(s.ptmx, &pty.Winsize{Cols: req.Cols, Rows: req.Rows}); err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.cols = req.Cols
	s.rows = req.Rows
	writeJSON(w, map[string]interface{}{"status": "ok", "cols": req.Cols, "rows": req.Rows})
}

func handlePTYList(w http.ResponseWriter, r *http.Request) {
	ptySessionsMu.RLock()
	defer ptySessionsMu.RUnlock()

	var list []map[string]interface{}
	for _, s := range ptySessions {
		s.clientsMu.Lock()
		numClients := len(s.clients)
		s.clientsMu.Unlock()
		list = append(list, map[string]interface{}{
			"id":         s.ID,
			"shell":      s.Shell,
			"cwd":        s.Cwd,
			"cols":       s.cols,
			"rows":       s.rows,
			"clients":    numClients,
			"created_at": s.createdAt.Format(time.RFC3339),
		})
	}
	if list == nil {
		list = []map[string]interface{}{}
	}
	writeJSON(w, list)
}

func handlePTYGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ptySessionsMu.RLock()
	s, ok := ptySessions[id]
	ptySessionsMu.RUnlock()
	if !ok {
		httpError(w, http.StatusNotFound, "pty session not found")
		return
	}

	s.clientsMu.Lock()
	numClients := len(s.clients)
	s.clientsMu.Unlock()

	writeJSON(w, map[string]interface{}{
		"id":         s.ID,
		"shell":      s.Shell,
		"cwd":        s.Cwd,
		"cols":       s.cols,
		"rows":       s.rows,
		"clients":    numClients,
		"created_at": s.createdAt.Format(time.RFC3339),
	})
}

func handlePTYDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	ptySessionsMu.Lock()
	s, ok := ptySessions[id]
	if ok {
		delete(ptySessions, id)
	}
	ptySessionsMu.Unlock()

	if !ok {
		httpError(w, http.StatusNotFound, "pty session not found")
		return
	}

	s.ptmx.Close()
	_ = s.cmd.Process.Kill()
	log.Printf("pty: killed session %s", id)
	w.WriteHeader(http.StatusNoContent)
}

// ── Ports ───────────────────────────────────────────────────────────────────

func handlePortList(w http.ResponseWriter, r *http.Request) {
	type portInfo struct {
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
		State    string `json:"state"`
	}

	var ports []portInfo

	// Parse /proc/net/tcp and /proc/net/tcp6 for listening ports.
	for _, proto := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile("/proc/net/" + proto)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 4 {
				continue
			}
			// State 0A = LISTEN
			if fields[3] != "0A" {
				continue
			}
			// Parse local address (hex port after colon).
			parts := strings.Split(fields[1], ":")
			if len(parts) != 2 {
				continue
			}
			port, err := strconv.ParseInt(parts[1], 16, 32)
			if err != nil || port == 0 {
				continue
			}
			// Skip our own daemon port.
			if int(port) == daemonPort {
				continue
			}
			ports = append(ports, portInfo{
				Port:     int(port),
				Protocol: proto,
				State:    "LISTEN",
			})
		}
	}

	// Deduplicate (tcp and tcp6 may both report same port).
	seen := make(map[int]bool)
	var unique []portInfo
	for _, p := range ports {
		if !seen[p.Port] {
			seen[p.Port] = true
			unique = append(unique, p)
		}
	}
	if unique == nil {
		unique = []portInfo{}
	}
	writeJSON(w, unique)
}

// ── Port check helper ──

func handlePortCheck(w http.ResponseWriter, r *http.Request) {
	portStr := r.URL.Query().Get("port")
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		httpError(w, http.StatusBadRequest, "valid port required")
		return
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	available := err != nil
	if conn != nil {
		conn.Close()
	}
	writeJSON(w, map[string]interface{}{"port": port, "available": available})
}

// ── Git ─────────────────────────────────────────────────────────────────────

func gitCmd(dir string, args ...string) (string, int) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return strings.TrimSpace(string(out)), exitCode
}

// gitCmdWithAuth runs a git command with optional credential injection.
// If username and password are provided, a one-shot credential helper is used.
func gitCmdWithAuth(dir, username, password string, args ...string) (string, int) {
	if username == "" || password == "" {
		return gitCmd(dir, args...)
	}
	// Prepend credential helper config args before the actual command args.
	helper := fmt.Sprintf("!f() { echo username=%s; echo password=%s; }; f", username, password)
	fullArgs := append([]string{"-c", "credential.helper=" + helper}, args...)
	return gitCmd(dir, fullArgs...)
}

func handleGitClone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Path     string `json:"path"`
		Branch   string `json:"branch"`
		Depth    int    `json:"depth"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		httpError(w, http.StatusBadRequest, "url required")
		return
	}
	if req.Path == "" {
		req.Path = "/workspace"
	}

	args := []string{"clone"}
	if req.Branch != "" {
		args = append(args, "-b", req.Branch)
	}
	if req.Depth > 0 {
		args = append(args, "--depth", fmt.Sprintf("%d", req.Depth))
	}
	args = append(args, req.URL, req.Path)

	out, exitCode := gitCmdWithAuth("", req.Username, req.Password, args...)
	writeJSON(w, map[string]interface{}{"output": out, "exit_code": exitCode, "path": req.Path})
}

func handleGitStatus(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "."
	}
	out, exitCode := gitCmd(dir, "status", "--porcelain")
	writeJSON(w, map[string]interface{}{"output": out, "exit_code": exitCode})
}

func handleGitCommit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Message string `json:"message"`
		AddAll  bool   `json:"add_all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Message == "" {
		httpError(w, http.StatusBadRequest, "message required")
		return
	}
	if req.Path == "" {
		req.Path = "."
	}

	if req.AddAll {
		gitCmd(req.Path, "add", "-A")
	}

	out, exitCode := gitCmd(req.Path, "commit", "-m", req.Message)
	writeJSON(w, map[string]interface{}{"output": out, "exit_code": exitCode})
}

func handleGitPush(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string `json:"path"`
		Remote   string `json:"remote"`
		Branch   string `json:"branch"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Path == "" {
		req.Path = "."
	}

	args := []string{"push"}
	if req.Remote != "" {
		args = append(args, req.Remote)
	}
	if req.Branch != "" {
		args = append(args, req.Branch)
	}

	out, exitCode := gitCmdWithAuth(req.Path, req.Username, req.Password, args...)
	writeJSON(w, map[string]interface{}{"output": out, "exit_code": exitCode})
}

func handleGitPull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string `json:"path"`
		Remote   string `json:"remote"`
		Branch   string `json:"branch"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Path == "" {
		req.Path = "."
	}

	args := []string{"pull"}
	if req.Remote != "" {
		args = append(args, req.Remote)
	}
	if req.Branch != "" {
		args = append(args, req.Branch)
	}

	out, exitCode := gitCmdWithAuth(req.Path, req.Username, req.Password, args...)
	writeJSON(w, map[string]interface{}{"output": out, "exit_code": exitCode})
}

func handleGitBranches(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "."
	}
	out, exitCode := gitCmd(dir, "branch", "-a", "--no-color")

	var branches []string
	for _, line := range strings.Split(out, "\n") {
		b := strings.TrimSpace(line)
		b = strings.TrimPrefix(b, "* ")
		if b != "" {
			branches = append(branches, b)
		}
	}
	writeJSON(w, map[string]interface{}{"branches": branches, "exit_code": exitCode})
}

func handleGitLog(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "."
	}
	limit := r.URL.Query().Get("limit")
	if limit == "" {
		limit = "20"
	}

	out, exitCode := gitCmd(dir, "log", "--oneline", "-n", limit, "--no-color")
	writeJSON(w, map[string]interface{}{"output": out, "exit_code": exitCode})
}

// ── Utilities ───────────────────────────────────────────────────────────────

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain")
	http.Error(w, strings.TrimSpace(msg), code)
}
