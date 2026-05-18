// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package integration contains end-to-end tests that spawn the
// real api-ratelimiter binary as a subprocess and exercise it via
// its public interfaces (unix socket /check, admin port, metrics
// port). Gated by `//go:build integration` so `go test ./...` stays
// fast; run with `go test -tags=integration ./test/integration/...`.
//
// See docs/specification.md §17.2 for the full plan.
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// binaryPath is set by TestMain to the path of the freshly-built
// api-ratelimiter binary. All tests reuse the same build to avoid
// paying compilation cost per test.
var binaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "api-ratelimiter-itest-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	binaryPath = filepath.Join(tmp, "api-ratelimiter")

	// Build from the module root. Tests live two directories deep:
	// test/integration/, so ../../ points at the module root.
	build := exec.Command("go", "build",
		"-trimpath",
		"-o", binaryPath,
		"./cmd/api-ratelimiter",
	)
	build.Dir = "../.."
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		_ = os.RemoveAll(tmp)
		fmt.Fprintln(os.Stderr, "build api-ratelimiter:", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
