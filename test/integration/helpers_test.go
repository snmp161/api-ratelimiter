// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// service is a running api-ratelimiter subprocess with all its
// listeners and an optional miniredis backing it. Cleanup runs
// automatically via t.Cleanup — tests don't have to defer stop().
type service struct {
	t           *testing.T
	cmd         *exec.Cmd
	socketPath  string // empty when --listen is TCP
	listen      string // raw --listen value, for diagnostics
	adminAddr   string // host:port
	metricsAddr string // host:port
	redis       *miniredis.Miniredis
	stdout      *bytes.Buffer
	stderr      *bytes.Buffer
}

// spawnOpts is set by SpawnOpt functions. spawnService applies it.
type spawnOpts struct {
	extraArgs  []string
	listen     string // override --listen (default unix:<tmpdir>/ratelimit.sock)
	socketMode string // default "0666"
	skipRedis  bool   // don't start miniredis; point --redis-addr at an unreachable target
	expectExit bool   // the process should exit on its own; don't waitReady
}

// SpawnOpt mutates spawnOpts.
type SpawnOpt func(*spawnOpts)

// withFlag appends an arbitrary --flag=value to the command line.
// pflag's "last wins" behaviour means this can override defaults set
// by spawnService.
func withFlag(name, value string) SpawnOpt {
	return func(o *spawnOpts) {
		o.extraArgs = append(o.extraArgs, name+"="+value)
	}
}

func withListen(spec string) SpawnOpt   { return func(o *spawnOpts) { o.listen = spec } }
func withSocketMode(m string) SpawnOpt  { return func(o *spawnOpts) { o.socketMode = m } }
func withoutRedis() SpawnOpt            { return func(o *spawnOpts) { o.skipRedis = true } }

// expectExit tells spawnService that the process is expected to fail
// at startup (validation error tests). spawnService will not waitReady;
// it returns once the process has exited, and exitCode() reflects the
// actual code.
func expectExit() SpawnOpt { return func(o *spawnOpts) { o.expectExit = true } }

func spawnService(t *testing.T, opts ...SpawnOpt) *service {
	t.Helper()

	o := spawnOpts{}
	for _, fn := range opts {
		fn(&o)
	}

	var mr *miniredis.Miniredis
	redisAddr := "127.0.0.1:1" // guaranteed-unreachable; force fail-open
	if !o.skipRedis {
		mr = miniredis.RunT(t)
		redisAddr = mr.Addr()
	}

	if o.listen == "" {
		o.listen = "unix:" + filepath.Join(t.TempDir(), "ratelimit.sock")
	}
	if o.socketMode == "" {
		o.socketMode = "0666"
	}

	adminPort := freePort(t)
	metricsPort := freePort(t)

	args := []string{
		"--listen=" + o.listen,
		"--socket-mode=" + o.socketMode,
		"--admin-listen=127.0.0.1:" + strconv.Itoa(adminPort),
		"--metrics-listen=127.0.0.1:" + strconv.Itoa(metricsPort),
		"--redis-addr=" + redisAddr,
		"--log-format=text",
		"--log-level=debug",
	}
	args = append(args, o.extraArgs...)

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
		listen:      o.listen,
		adminAddr:   fmt.Sprintf("127.0.0.1:%d", adminPort),
		metricsAddr: fmt.Sprintf("127.0.0.1:%d", metricsPort),
		redis:       mr,
		stdout:      stdout,
		stderr:      stderr,
	}
	if p, ok := unixPathOf(o.listen); ok {
		s.socketPath = p
	}

	t.Cleanup(s.stop)

	if o.expectExit {
		s.waitForExit(5 * time.Second)
	} else {
		s.waitReady(5 * time.Second)
	}
	return s
}

// waitReady polls /healthz until 200 or timeout. On timeout dumps
// stdout/stderr.
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

// waitForExit blocks until the process exits or the deadline expires.
// For validation-error tests.
func (s *service) waitForExit(timeout time.Duration) {
	s.t.Helper()
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.t.Fatalf("service did not exit within %v (expected non-zero exit)\n--- stderr ---\n%s",
			timeout, s.stderr.String())
	}
}

// exitCode returns the process exit code. Returns -1 if the process
// is still running or was signalled.
func (s *service) exitCode() int {
	if s.cmd.ProcessState == nil {
		return -1
	}
	return s.cmd.ProcessState.ExitCode()
}

// stop sends SIGTERM, waits up to 15s for graceful exit, then SIGKILL.
// Idempotent.
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

// check sends an HTTP GET to /check using the unix socket (or
// configured TCP listener), mimicking nginx's auth_request request
// shape. Returns the status code.
func (s *service) check(headers map[string]string) (int, error) {
	var (
		transport *http.Transport
		target    string
	)
	if s.socketPath != "" {
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", s.socketPath)
			},
		}
		target = "unix"
	} else {
		host := strings.TrimPrefix(s.listen, "tcp://")
		transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", host)
			},
		}
		target = host
	}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "http://"+target+"/check", nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	return resp.StatusCode, nil
}

// adminGET issues a GET against the admin port. Caller must close
// the response body.
func (s *service) adminGET(path string) (*http.Response, error) {
	return http.Get("http://" + s.adminAddr + path)
}

// metricsGET fetches /metrics and returns the raw body.
func (s *service) metricsGET() (string, int, error) {
	resp, err := http.Get("http://" + s.metricsAddr + "/metrics")
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}

// stdoutSnapshot / stderrSnapshot return the current contents of the
// captured streams. Reading is safe — bytes.Buffer.String() copies
// internally, but bytes.Buffer is not goroutine-safe for concurrent
// writes; this assumes the test isn't generating live traffic at the
// moment of the read.
func (s *service) stdoutSnapshot() string { return s.stdout.String() }
func (s *service) stderrSnapshot() string { return s.stderr.String() }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func unixPathOf(spec string) (string, bool) {
	const prefix = "unix:"
	if strings.HasPrefix(spec, prefix) {
		return strings.TrimPrefix(spec, prefix), true
	}
	return "", false
}
