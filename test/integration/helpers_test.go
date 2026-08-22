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
	"net/url"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// syncBuffer is a mutex-guarded bytes.Buffer. exec.Cmd copies the
// child's stdout/stderr from a pipe in its own goroutine, so a test
// reading the captured output while the process is still logging would
// otherwise race (visible under -race, and a real torn read without).
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

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
	redisAddr   string // host:port the service was pointed at
	redis       *miniredis.Miniredis
	stdout      *syncBuffer
	stderr      *syncBuffer

	clientOnce sync.Once
	client     *http.Client // /check client, connection-reusing

	dbMu sync.Mutex
	dbs  map[int]*redis.Client
}

// spawnOpts is set by SpawnOpt functions. spawnService applies it.
type spawnOpts struct {
	extraArgs  []string
	listen     string // override --listen (default unix:<tmpdir>/ratelimit.sock)
	socketMode string // default "0666"
	skipRedis  bool   // don't start miniredis; point --redis-addr at an unreachable target
	redisAddr  string // point at an externally-managed Redis (see ctlRedis)
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

func withListen(spec string) SpawnOpt  { return func(o *spawnOpts) { o.listen = spec } }
func withSocketMode(m string) SpawnOpt { return func(o *spawnOpts) { o.socketMode = m } }
func withoutRedis() SpawnOpt           { return func(o *spawnOpts) { o.skipRedis = true } }

// withRedisAddr points the service at a Redis the test controls (see
// newCtlRedis) instead of the per-service miniredis, so the test can
// stop and restart it mid-run.
func withRedisAddr(addr string) SpawnOpt { return func(o *spawnOpts) { o.redisAddr = addr } }

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
	switch {
	case o.redisAddr != "":
		redisAddr = o.redisAddr
	case !o.skipRedis:
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

	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
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
		redisAddr:   redisAddr,
		redis:       mr,
		stdout:      stdout,
		stderr:      stderr,
		dbs:         map[int]*redis.Client{},
	}
	if p, ok := unixPathOf(o.listen); ok {
		s.socketPath = p
	}

	t.Cleanup(s.stop)
	t.Cleanup(s.closeDBs)

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

// signalAndWait sends sig and waits for the process to exit, returning
// how long it took. Fails the test if the process outlives timeout.
func (s *service) signalAndWait(sig syscall.Signal, timeout time.Duration) time.Duration {
	s.t.Helper()
	start := time.Now()
	if err := s.cmd.Process.Signal(sig); err != nil {
		s.t.Fatalf("signal %v: %v", sig, err)
	}
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(timeout):
		_ = s.cmd.Process.Kill()
		<-done
		s.t.Fatalf("process did not exit within %v after %v\n--- stdout ---\n%s",
			timeout, sig, s.stdout.String())
		return 0
	}
}

// ───── /check client ───────────────────────────────────────────────────

// checkClient returns a connection-reusing client bound to the
// service's /check listener (unix socket or TCP). Cached so tests that
// fire thousands of requests don't build a transport per call.
func (s *service) checkClient() *http.Client {
	s.clientOnce.Do(func() {
		network, addr := "tcp", strings.TrimPrefix(s.listen, "tcp://")
		if s.socketPath != "" {
			network, addr = "unix", s.socketPath
		}
		s.client = &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
				MaxIdleConns:        256,
				MaxIdleConnsPerHost: 256,
			},
			Timeout: 5 * time.Second,
		}
	})
	return s.client
}

// checkHost is the host part of the /check URL. Meaningless for a unix
// socket (the dialer ignores it) but has to be syntactically valid.
func (s *service) checkHost() string {
	if s.socketPath != "" {
		return "unix"
	}
	return strings.TrimPrefix(s.listen, "tcp://")
}

// doCheck issues an arbitrary request against the /check listener.
// target is a path with an optional query ("/check?api_key=x").
// Returns the status code and the response body.
func (s *service) doCheck(method, target string, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(method, "http://"+s.checkHost()+target, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.checkClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// check sends a GET /check mimicking nginx's auth_request subrequest
// shape and returns the status code.
func (s *service) check(headers map[string]string) (int, error) {
	code, _, err := s.doCheck(http.MethodGet, "/check", headers)
	return code, err
}

// mustCheck is check() with the error folded into t.Fatalf — most call
// sites only care about the status code.
func (s *service) mustCheck(headers map[string]string) int {
	s.t.Helper()
	code, err := s.check(headers)
	if err != nil {
		s.t.Fatalf("check: %v", err)
	}
	return code
}

// checkN sends n sequential /check requests and returns the statuses in
// order.
func (s *service) checkN(n int, headers map[string]string) []int {
	s.t.Helper()
	out := make([]int, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s.mustCheck(headers))
	}
	return out
}

// countStatus counts occurrences of want in a status slice.
func countStatus(codes []int, want int) int {
	n := 0
	for _, c := range codes {
		if c == want {
			n++
		}
	}
	return n
}

// ───── Redis access (test side) ────────────────────────────────────────

// db returns a go-redis client for the given logical DB of whatever
// Redis the service is pointed at. Clients are cached and closed by
// t.Cleanup.
func (s *service) db(n int) *redis.Client {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	if c, ok := s.dbs[n]; ok {
		return c
	}
	c := redis.NewClient(&redis.Options{
		Addr:        s.redisAddr,
		DB:          n,
		DialTimeout: time.Second,
	})
	s.dbs[n] = c
	return c
}

func (s *service) closeDBs() {
	s.dbMu.Lock()
	defer s.dbMu.Unlock()
	for _, c := range s.dbs {
		_ = c.Close()
	}
	s.dbs = map[int]*redis.Client{}
}

// seedLimit writes an operator-managed per-key limit into redisDB1,
// matching the schema the service reads (`rate:limit:<api_key>`).
func (s *service) seedLimit(apiKey string, limit int64) {
	s.t.Helper()
	ctx := context.Background()
	err := s.db(1).HSet(ctx, "rate:limit:"+apiKey, map[string]any{
		"limit":      limit,
		"created_at": time.Now().Unix(),
	}).Err()
	if err != nil {
		s.t.Fatalf("seed rate:limit:%s: %v", apiKey, err)
	}
}

// seedAbuse writes an abuse record directly into redisDB2/redisDB3 so
// admin tests have rows to render without having to generate traffic.
func (s *service) seedAbuse(db int, prefix, key string, ttl time.Duration) {
	s.t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	full := prefix + key
	err := s.db(db).HSet(ctx, full, map[string]any{
		"first_seen":     now - 60,
		"last_seen":      now,
		"total_requests": 500,
		"burst_hits":     5,
		"abuse_hits":     7,
	}).Err()
	if err != nil {
		s.t.Fatalf("seed %s: %v", full, err)
	}
	if err := s.db(db).Expire(ctx, full, ttl).Err(); err != nil {
		s.t.Fatalf("expire %s: %v", full, err)
	}
}

func (s *service) seedAbuseKey(apiKey string) {
	s.t.Helper()
	s.seedAbuse(2, "rate:abuse:key:", apiKey, 10*time.Minute)
}

func (s *service) seedAbuseIP(ip string) {
	s.t.Helper()
	s.seedAbuse(3, "rate:abuse:ip:", ip, 10*time.Minute)
}

// hgetAll reads a hash from the given logical DB; ok is false when the
// key does not exist.
func (s *service) hgetAll(db int, key string) (map[string]string, bool) {
	s.t.Helper()
	vals, err := s.db(db).HGetAll(context.Background(), key).Result()
	if err != nil {
		s.t.Fatalf("hgetall %s (db%d): %v", key, db, err)
	}
	return vals, len(vals) > 0
}

// keys lists keys matching a pattern in the given logical DB.
func (s *service) keys(db int, pattern string) []string {
	s.t.Helper()
	out, err := s.db(db).Keys(context.Background(), pattern).Result()
	if err != nil {
		s.t.Fatalf("keys %s (db%d): %v", pattern, db, err)
	}
	sort.Strings(out)
	return out
}

func (s *service) ttl(db int, key string) time.Duration {
	s.t.Helper()
	d, err := s.db(db).TTL(context.Background(), key).Result()
	if err != nil {
		s.t.Fatalf("ttl %s (db%d): %v", key, db, err)
	}
	return d
}

// hfieldInt reads a numeric hash field, failing the test if it is
// missing or unparseable.
func hfieldInt(t *testing.T, vals map[string]string, field string) int64 {
	t.Helper()
	raw, ok := vals[field]
	if !ok {
		t.Fatalf("field %q missing from %v", field, vals)
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("field %q = %q is not an int64: %v", field, raw, err)
	}
	return v
}

// ctlRedis is a miniredis the test can stop and restart on a stable
// address — used to exercise Redis-down / Redis-recovered paths that a
// t-scoped miniredis.RunT can't express. Restarting starts from an
// empty dataset, same as a real Redis that lost its data.
type ctlRedis struct {
	t    *testing.T
	addr string
	mu   sync.Mutex
	mr   *miniredis.Miniredis
}

func newCtlRedis(t *testing.T) *ctlRedis {
	t.Helper()
	c := &ctlRedis{t: t, addr: fmt.Sprintf("127.0.0.1:%d", freePort(t))}
	c.start()
	t.Cleanup(c.stop)
	return c
}

func (c *ctlRedis) start() {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mr != nil {
		return
	}
	mr := miniredis.NewMiniRedis()
	if err := mr.StartAddr(c.addr); err != nil {
		c.t.Fatalf("start miniredis on %s: %v", c.addr, err)
	}
	c.mr = mr
}

func (c *ctlRedis) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mr == nil {
		return
	}
	c.mr.Close()
	c.mr = nil
}

// ───── admin / metrics HTTP ────────────────────────────────────────────

// adminGET issues a GET against the admin port. Caller must close
// the response body.
func (s *service) adminGET(path string) (*http.Response, error) {
	return http.Get("http://" + s.adminAddr + path)
}

// adminBody GETs an admin page and returns status + body.
func (s *service) adminBody(path string) (int, string) {
	s.t.Helper()
	resp, err := s.adminGET(path)
	if err != nil {
		s.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, string(b)
}

// adminDoc GETs an admin page and parses it with goquery. Fails the
// test on a non-200 or unparseable body.
func (s *service) adminDoc(path string) *goquery.Document {
	s.t.Helper()
	code, body := s.adminBody(path)
	if code != http.StatusOK {
		s.t.Fatalf("GET %s: status = %d, want 200\n%s", path, code, body)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		s.t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// adminClient does not follow redirects — admin actions answer 303 and
// the test wants to see it rather than the redirect target.
func adminClient() *http.Client {
	return &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// adminPOST posts a form to the admin port without following the
// redirect. Returns status and body.
func (s *service) adminPOST(path string, form url.Values) (int, string) {
	s.t.Helper()
	resp, err := adminClient().PostForm("http://"+s.adminAddr+path, form)
	if err != nil {
		s.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatalf("read POST %s: %v", path, err)
	}
	return resp.StatusCode, string(b)
}

// csrfToken pulls the hidden csrf_token input out of a rendered admin
// page — the same way a browser would before submitting the form.
func (s *service) csrfToken(path string) string {
	s.t.Helper()
	doc := s.adminDoc(path)
	tok, ok := doc.Find("input[name=csrf_token]").First().Attr("value")
	if !ok || tok == "" {
		s.t.Fatalf("no csrf_token input on %s", path)
	}
	return tok
}

// tableKeys pulls the first <code> cell out of every row of a table —
// on every admin list page that cell holds the api_key / IP.
func tableKeys(sel *goquery.Selection) []string {
	var out []string
	sel.Find("tr").Each(func(_ int, tr *goquery.Selection) {
		c := tr.Find("code").First()
		if c.Length() == 0 {
			return
		}
		out = append(out, strings.TrimSpace(c.Text()))
	})
	return out
}

// topKeys returns the keys listed in the page's in-memory Top-N table
// (the one under the "… (in-memory)" heading). Empty when the page
// reports no active counters.
//
// Substring-matching the whole page can't be used for this: the same
// key usually also appears in the Redis-backed table below.
func (s *service) topKeys(path string) []string {
	s.t.Helper()
	doc := s.adminDoc(path)
	var out []string
	doc.Find("h2").Each(func(_ int, h *goquery.Selection) {
		if !strings.Contains(h.Text(), "in-memory") {
			return
		}
		out = append(out, tableKeys(h.NextAllFiltered("table").First())...)
	})
	return out
}

// topRowCells returns the cell texts of the in-memory top-table row
// whose key cell equals key, or nil when the key is not listed. Column
// order is key, total, burst, violations/abuse.
func (s *service) topRowCells(path, key string) []string {
	s.t.Helper()
	doc := s.adminDoc(path)
	var cells []string
	doc.Find("h2").Each(func(_ int, h *goquery.Selection) {
		if !strings.Contains(h.Text(), "in-memory") || cells != nil {
			return
		}
		h.NextAllFiltered("table").First().Find("tr").EachWithBreak(func(_ int, tr *goquery.Selection) bool {
			if strings.TrimSpace(tr.Find("code").First().Text()) != key {
				return true
			}
			tr.Find("td").Each(func(_ int, td *goquery.Selection) {
				cells = append(cells, strings.TrimSpace(td.Text()))
			})
			return false
		})
	})
	return cells
}

// topTotal returns the "total" column of the in-memory top-table row
// for key, or -1 when the counter is not listed.
func (s *service) topTotal(path, key string) int64 {
	s.t.Helper()
	cells := s.topRowCells(path, key)
	if len(cells) < 2 {
		return -1
	}
	v, err := strconv.ParseInt(cells[1], 10, 64)
	if err != nil {
		s.t.Fatalf("total cell for %s on %s = %q: %v", key, path, cells[1], err)
	}
	return v
}

// storedKeys returns the keys of the Redis-backed table (the one
// wrapped in the delete/purge action form).
func (s *service) storedKeys(path string) []string {
	s.t.Helper()
	return tableKeys(s.adminDoc(path).Find("form#action-form"))
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
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

// parseMetrics turns a Prometheus exposition body into a
// series-name → value map, where the series name is the full
// `name{label="value",…}` string as exposed (labels are already sorted
// by the client library).
func parseMetrics(body string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndex(line, " ")
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			continue
		}
		out[line[:i]] = v
	}
	return out
}

// seriesName renders `name{k="v",…}` with labels sorted, matching the
// exposition format so it can be used as a map key.
func seriesName(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", k, labels[k]))
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// metric scrapes /metrics and returns one series' value. Missing series
// read as 0 — Prometheus counters are only exposed once touched, and
// "not incremented yet" is exactly what a test asserting 0 means.
func (s *service) metric(name string, labels map[string]string) float64 {
	s.t.Helper()
	body, code, err := s.metricsGET()
	if err != nil {
		s.t.Fatalf("/metrics: %v", err)
	}
	if code != http.StatusOK {
		s.t.Fatalf("/metrics status = %d, want 200", code)
	}
	return parseMetrics(body)[seriesName(name, labels)]
}

// metricSeries reports whether a series is present in the exposition at
// all — distinct from "present with value 0".
func (s *service) metricExists(name string, labels map[string]string) bool {
	s.t.Helper()
	body, _, err := s.metricsGET()
	if err != nil {
		s.t.Fatalf("/metrics: %v", err)
	}
	_, ok := parseMetrics(body)[seriesName(name, labels)]
	return ok
}

// ───── polling / log helpers ───────────────────────────────────────────

// waitFor polls cond every 25ms until it returns true or the timeout
// expires, in which case the test fails with what.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

// stdoutSnapshot / stderrSnapshot return the current contents of the
// captured streams.
func (s *service) stdoutSnapshot() string { return s.stdout.String() }
func (s *service) stderrSnapshot() string { return s.stderr.String() }

// waitLog blocks until substr shows up in the service's stdout.
func (s *service) waitLog(substr string, timeout time.Duration) {
	s.t.Helper()
	waitFor(s.t, timeout, fmt.Sprintf("log line containing %q", substr), func() bool {
		return strings.Contains(s.stdoutSnapshot(), substr)
	})
}

// waitCleanupRuns blocks until the service has completed at least n
// cleanup cycles since start (counted from the "cleanup finished" log,
// which is emitted at the end of every cycle including the final one).
func (s *service) waitCleanupRuns(n int, timeout time.Duration) {
	s.t.Helper()
	waitFor(s.t, timeout, fmt.Sprintf("%d cleanup cycles", n), func() bool {
		return strings.Count(s.stdoutSnapshot(), "cleanup finished") >= n
	})
}

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

// sleepToNextSecond blocks until just after the next whole second,
// plus extra. Tests that need all their requests inside one 1s slot
// start from a boundary instead of hoping they don't straddle one.
func sleepToNextSecond(extra time.Duration) {
	next := time.Now().Truncate(time.Second).Add(time.Second + extra)
	time.Sleep(time.Until(next))
}

// sameWindow reports whether two instants fall into the same rate-limit
// slot of the given width. Tests that depend on "all requests landed in
// one window" use it to skip rather than flake when they straddle a
// boundary.
func sameWindow(start, end time.Time, windowSeconds int64) bool {
	return start.Unix()/windowSeconds == end.Unix()/windowSeconds
}
