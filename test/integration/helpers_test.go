// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// service is a running api-ratelimiter subprocess with all its
// listeners and a miniredis backing it. Cleanup runs automatically
// via t.Cleanup — tests don't have to defer stop().
//
// This is the minimal skeleton: spawn defaults, wait until ready,
// expose admin/metrics HTTP. Subsequent integration stages will grow
// the fixture as needed (functional options for extra flags, a
// /check helper over the unix socket, exit-code capture for
// validation tests, etc.).
type service struct {
	t           *testing.T
	cmd         *exec.Cmd
	adminAddr   string // host:port
	metricsAddr string // host:port
	redis       *miniredis.Miniredis
	stdout      *bytes.Buffer
	stderr      *bytes.Buffer
}

// spawnService starts the binary against a fresh miniredis, waits
// for /healthz to return 200, and registers cleanup. Each test gets
// its own subprocess and isolated state.
func spawnService(t *testing.T) *service {
	t.Helper()

	mr := miniredis.RunT(t)

	socketPath := filepath.Join(t.TempDir(), "ratelimit.sock")
	adminPort := freePort(t)
	metricsPort := freePort(t)

	args := []string{
		"--listen=unix:" + socketPath,
		"--socket-mode=0666",
		"--admin-listen=127.0.0.1:" + strconv.Itoa(adminPort),
		"--metrics-listen=127.0.0.1:" + strconv.Itoa(metricsPort),
		"--redis-addr=" + mr.Addr(),
		"--log-format=text",
		"--log-level=debug",
	}

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd := exec.Command(binaryPath, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start binary: %v\nargs: %v", err, args)
	}

	s := &service{
		t:           t,
		cmd:         cmd,
		adminAddr:   fmt.Sprintf("127.0.0.1:%d", adminPort),
		metricsAddr: fmt.Sprintf("127.0.0.1:%d", metricsPort),
		redis:       mr,
		stdout:      stdout,
		stderr:      stderr,
	}
	t.Cleanup(s.stop)
	s.waitReady(5 * time.Second)
	return s
}

// waitReady polls /healthz until 200 or timeout. On timeout dumps
// captured stdout/stderr to make CI failures debuggable.
func (s *service) waitReady(timeout time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + s.adminAddr + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body) //nolint:errcheck
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("service did not become ready within %v\n--- stdout ---\n%s\n--- stderr ---\n%s",
		timeout, s.stdout.String(), s.stderr.String())
}

// stop sends SIGTERM, waits up to 15s for graceful exit, then SIGKILL.
// Idempotent — safe to call multiple times.
func (s *service) stop() {
	if s.cmd.Process == nil {
		return
	}
	if s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited() {
		return
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)

	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

// adminGET issues a GET against the admin port. Caller must close
// the response body.
func (s *service) adminGET(path string) (*http.Response, error) {
	return http.Get("http://" + s.adminAddr + path)
}

// metricsGET fetches the /metrics page and returns the raw body.
func (s *service) metricsGET() (string, int, error) {
	resp, err := http.Get("http://" + s.metricsAddr + "/metrics")
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}

// freePort grabs and releases a TCP port. There's a tiny race window
// between release and the subprocess re-binding — tests do not
// parallelise at a level where this would matter.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
