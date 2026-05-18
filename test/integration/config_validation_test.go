// SPDX-License-Identifier: Apache-2.0

//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestValidation_FlagsRejected runs the full list of bad-flag cases
// from spec §17.2 A. Each case must:
//   - exit with a non-zero code (the binary's main() calls os.Exit(1)
//     on Validate() failure; pflag itself exits with code 2 on
//     unknown-flag / type-parse error, which is also acceptable)
//   - print something diagnostic to stderr (substring match)
//
// Table-driven so adding a 13th rejected combination is a single
// struct literal.
func TestValidation_FlagsRejected(t *testing.T) {
	cases := []struct {
		name string
		opts []SpawnOpt
		// stderrContains: substring expected in the binary's stderr.
		// Empty string skips the substring check (when pflag emits an
		// opaque parse error that's not stable across versions).
		stderrContains string
	}{
		{
			name: "burst >= global * multiplier",
			opts: []SpawnOpt{
				withFlag("--global-limit", "10"),
				withFlag("--abuse-multiplier", "10"),
				withFlag("--burst", "100"),
			},
			stderrContains: "burst",
		},
		{
			name: "global-limit zero",
			opts: []SpawnOpt{withFlag("--global-limit", "0")},
			stderrContains: "global-limit",
		},
		{
			name: "global-limit negative",
			opts: []SpawnOpt{withFlag("--global-limit", "-1")},
			stderrContains: "global-limit",
		},
		{
			name: "burst negative",
			opts: []SpawnOpt{withFlag("--burst", "-1")},
			stderrContains: "burst",
		},
		{
			name: "cleanup-interval zero",
			opts: []SpawnOpt{withFlag("--cleanup-interval", "0s")},
			stderrContains: "cleanup-interval",
		},
		{
			name: "abuse-ttl zero",
			opts: []SpawnOpt{withFlag("--abuse-ttl", "0s")},
			stderrContains: "abuse-ttl",
		},
		{
			name: "abuse-multiplier zero",
			opts: []SpawnOpt{withFlag("--abuse-multiplier", "0")},
			stderrContains: "abuse-multiplier",
		},
		{
			name: "abuse-transfer-threshold zero",
			opts: []SpawnOpt{withFlag("--abuse-transfer-threshold", "0")},
			stderrContains: "abuse-transfer-threshold",
		},
		{
			name: "log-level invalid",
			opts: []SpawnOpt{withFlag("--log-level", "trace")},
			stderrContains: "log-level",
		},
		{
			name: "log-format invalid",
			opts: []SpawnOpt{withFlag("--log-format", "xml")},
			stderrContains: "log-format",
		},
		{
			name: "window invalid",
			opts: []SpawnOpt{withFlag("--window", "hour")},
			stderrContains: "window",
		},
		{
			name: "socket-mode > 0777",
			opts: []SpawnOpt{withSocketMode("1000")},
			stderrContains: "socket-mode",
		},
		{
			name: "socket-mode non-octal",
			opts: []SpawnOpt{withSocketMode("abc")},
			stderrContains: "socket-mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]SpawnOpt{expectExit()}, tc.opts...)
			s := spawnService(t, opts...)

			if code := s.exitCode(); code == 0 {
				t.Errorf("expected non-zero exit code, got 0\nstderr:\n%s",
					s.stderrSnapshot())
			}
			if tc.stderrContains != "" &&
				!strings.Contains(s.stderrSnapshot(), tc.stderrContains) {
				t.Errorf("stderr should mention %q, got:\n%s",
					tc.stderrContains, s.stderrSnapshot())
			}
		})
	}
}
