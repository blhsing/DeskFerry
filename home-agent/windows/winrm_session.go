//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const defaultWinRMSessionIdleTimeout = 5 * time.Minute

type winRMRequest struct {
	ID       uint64 `json:"id"`
	Action   string `json:"action"`
	Key      string `json:"key,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Command  string `json:"command,omitempty"`
	Port     string `json:"port,omitempty"`
}

type winRMResponse struct {
	ID     uint64 `json:"id"`
	OK     bool   `json:"ok"`
	Output string `json:"output"`
	Error  string `json:"error"`
	Reused bool   `json:"reused"`
}

type winRMSessionManager struct {
	mu          sync.Mutex
	idleTimeout time.Duration
	lastUsed    time.Time
	idleTimer   *time.Timer
	nextID      uint64
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	stdout      *bufio.Reader
}

func newWinRMSessionManager(idleTimeout time.Duration) *winRMSessionManager {
	if idleTimeout <= 0 {
		idleTimeout = defaultWinRMSessionIdleTimeout
	}
	return &winRMSessionManager{idleTimeout: idleTimeout}
}

func winRMSessionKey(destination, user, port, password string) string {
	digest := sha256.Sum256([]byte(password))
	return strings.Join([]string{strings.TrimSpace(destination), strings.TrimSpace(user), strings.TrimSpace(port), hex.EncodeToString(digest[:])}, "\x00")
}

func (m *winRMSessionManager) Execute(ctx context.Context, destination, user, password, command, port string) (winRMResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return winRMResponse{}, err
	}
	if err := m.ensureWorkerLocked(); err != nil {
		return winRMResponse{}, err
	}
	m.nextID++
	request := winRMRequest{
		ID: m.nextID, Action: "execute",
		Key:  winRMSessionKey(destination, user, port, password),
		User: user, Password: password, Command: command, Port: port,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return winRMResponse{}, err
	}
	if _, err := m.stdin.Write(append(payload, '\n')); err != nil {
		m.stopWorkerLocked()
		return winRMResponse{}, fmt.Errorf("send command to persistent PowerShell worker: %w", err)
	}

	type readResult struct {
		line string
		err  error
	}
	result := make(chan readResult, 1)
	go func(reader *bufio.Reader) {
		line, readErr := reader.ReadString('\n')
		result <- readResult{line: line, err: readErr}
	}(m.stdout)

	var read readResult
	select {
	case <-ctx.Done():
		m.stopWorkerLocked()
		return winRMResponse{}, ctx.Err()
	case read = <-result:
	}
	if read.err != nil {
		m.stopWorkerLocked()
		return winRMResponse{}, fmt.Errorf("read persistent PowerShell worker response: %w", read.err)
	}
	var response winRMResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(read.line)), &response); err != nil {
		m.stopWorkerLocked()
		return winRMResponse{}, fmt.Errorf("decode persistent PowerShell worker response: %w", err)
	}
	if response.ID != request.ID {
		m.stopWorkerLocked()
		return winRMResponse{}, fmt.Errorf("persistent PowerShell worker returned response %d for request %d", response.ID, request.ID)
	}
	m.touchLocked()
	if !response.OK {
		return response, errors.New(response.Error)
	}
	return response, nil
}

func (m *winRMSessionManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
		m.idleTimer = nil
	}
	m.stopWorkerLocked()
}

func (m *winRMSessionManager) ensureWorkerLocked() error {
	if m.cmd != nil && m.cmd.Process != nil {
		return nil
	}
	encoded := encodePowerShellCommand(winRMSessionWorkerScript)
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open persistent PowerShell worker input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open persistent PowerShell worker output: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start persistent PowerShell worker: %w", err)
	}
	m.cmd = cmd
	m.stdin = stdin
	m.stdout = bufio.NewReader(stdout)
	return nil
}

func (m *winRMSessionManager) touchLocked() {
	m.lastUsed = time.Now()
	if m.idleTimer != nil {
		m.idleTimer.Stop()
	}
	m.idleTimer = time.AfterFunc(m.idleTimeout, m.expireIdleWorker)
}

func (m *winRMSessionManager) expireIdleWorker() {
	m.mu.Lock()
	defer m.mu.Unlock()
	remaining := m.idleTimeout - time.Since(m.lastUsed)
	if remaining > 0 {
		m.idleTimer = time.AfterFunc(remaining, m.expireIdleWorker)
		return
	}
	m.idleTimer = nil
	m.stopWorkerLocked()
}

func (m *winRMSessionManager) stopWorkerLocked() {
	if m.cmd == nil {
		return
	}
	if m.stdin != nil {
		payload, _ := json.Marshal(winRMRequest{Action: "close"})
		_, _ = m.stdin.Write(append(payload, '\n'))
		_ = m.stdin.Close()
	}
	cmd := m.cmd
	m.cmd = nil
	m.stdin = nil
	m.stdout = nil
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}
}

func encodePowerShellCommand(script string) string {
	utf16 := make([]byte, len([]rune(script))*2)
	for index, value := range []rune(script) {
		binary.LittleEndian.PutUint16(utf16[index*2:], uint16(value))
	}
	return base64.StdEncoding.EncodeToString(utf16)
}

const winRMSessionWorkerScript = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$session = $null
$sessionKey = ''
try {
    while (($line = [Console]::In.ReadLine()) -ne $null) {
        $request = $line | ConvertFrom-Json
        if ([string]$request.action -eq 'close') { break }
        if ([string]$request.action -eq 'ping') {
            [Console]::Out.WriteLine(([ordered]@{ id = [uint64]$request.id; ok = $true; output = 'ready'; error = ''; reused = $false } | ConvertTo-Json -Compress))
            [Console]::Out.Flush()
            continue
        }
        $response = [ordered]@{ id = [uint64]$request.id; ok = $false; output = ''; error = ''; reused = $false }
        try {
            $canReuse = $session -ne $null -and $sessionKey -eq [string]$request.key -and $session.State -eq 'Opened'
            if (-not $canReuse) {
                if ($session -ne $null) { Remove-PSSession -Session $session -ErrorAction SilentlyContinue }
                $secure = ConvertTo-SecureString ([string]$request.password) -AsPlainText -Force
                $credential = [Management.Automation.PSCredential]::new([string]$request.user, $secure)
                $session = New-PSSession -ComputerName localhost -Port ([int]$request.port) -Authentication Negotiate -Credential $credential
                $sessionKey = [string]$request.key
            } else {
                $response.reused = $true
            }
            $response.output = Invoke-Command -Session $session -ScriptBlock ([ScriptBlock]::Create([string]$request.command)) | Out-String -Width 240
            $response.ok = $true
        } catch {
            $response.error = $_.Exception.Message
            if ($session -ne $null) { Remove-PSSession -Session $session -ErrorAction SilentlyContinue }
            $session = $null
            $sessionKey = ''
        }
        [Console]::Out.WriteLine(($response | ConvertTo-Json -Compress))
        [Console]::Out.Flush()
    }
} finally {
    if ($session -ne $null) { Remove-PSSession -Session $session -ErrorAction SilentlyContinue }
}
`
