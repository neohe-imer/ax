// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/ax/internal/workspace"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"google.golang.org/protobuf/types/known/structpb"
)

// launcherScript is a stand-in for an image's ax-launcher: it copies every
// launcher contract file into the output file, one per line, and counts its
// runs in runs.
const launcherScript = `#!/bin/sh
echo run >> "$(dirname "$0")/runs"
{
  cat "$AX_GOAL_FILE"; echo
  cat "$AX_CONFIG_FILE"; echo
  cat "$AX_MCP_CONFIG_FILE"; echo
  cat "$AX_SESSION_FILE"; echo
} > "$AX_OUTPUT_FILE"
`

// withLauncher points h at a fake launcher and a temp run dir, gives the task
// a goal, and returns the launcher's directory.
func withLauncher(t *testing.T, h *harness, goal string) string {
	t.Helper()
	dir := t.TempDir()
	launcher := filepath.Join(dir, "ax-launcher")
	if err := os.WriteFile(launcher, []byte(launcherScript), 0o755); err != nil {
		t.Fatal(err)
	}
	h.cfg.LauncherPath = launcher
	h.cfg.RunDir = filepath.Join(dir, "run")
	h.task.Spec.Goal = &v1alpha1.Goal{Text: goal}
	return dir
}

func runs(t *testing.T, launcherDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(launcherDir, "runs"))
	if os.IsNotExist(err) {
		return 0
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "run")
}

func TestRun_StartsLauncherWithContractFiles(t *testing.T) {
	h := newHarness(t)
	withLauncher(t, h, "Summarize the repo.")
	config, err := structpb.NewStruct(map[string]any{"model": "fast"})
	if err != nil {
		t.Fatal(err)
	}
	h.task.Spec.Config = config
	h.task.Spec.SessionId = "s-1"
	h.cfg.Workspace = &v1alpha1.Workspace{
		Metadata: &v1alpha1.ObjectMeta{Name: "ws"},
		Spec: &v1alpha1.WorkspaceSpec{Mcp: &v1alpha1.MCPConfig{
			Servers: []*v1alpha1.MCPServer{{Name: "docs", Endpoint: "https://example.com/mcp"}},
		}},
	}

	if exit := h.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	out, err := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output"))
	if err != nil {
		t.Fatalf("launcher did not write $AX_OUTPUT_FILE under the workspace: %v", err)
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 4 {
		t.Fatalf("output = %q, want goal, config, MCP config and session", out)
	}
	if lines[0] != "Summarize the repo." {
		t.Errorf("goal file = %q", lines[0])
	}
	var gotConfig map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &gotConfig); err != nil || gotConfig["model"] != "fast" {
		t.Errorf("config file = %q (%v), want spec.config", lines[1], err)
	}
	var gotMCP struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &gotMCP); err != nil || gotMCP.MCPServers["docs"]["url"] != "https://example.com/mcp" {
		t.Errorf("MCP config file = %q (%v), want the workspace's servers", lines[2], err)
	}
	if lines[3] != "s-1" {
		t.Errorf("session file = %q, want s-1", lines[3])
	}
}

// Without a goal the launcher is not started, so the sandbox stays up for
// users to attach to.
func TestRun_NoGoalDoesNotStartLauncher(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "")
	h.start(t)

	waitHealthy(t, h.cfg.Port)
	time.Sleep(200 * time.Millisecond)
	if n := runs(t, dir); n != 0 {
		t.Errorf("launcher ran %d times without a goal, want 0", n)
	}
}

// A sandbox resumed after the launcher finished does not run the same goal
// again, but does run a new one.
func TestRun_DoesNotRestartFinishedLauncher(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "first goal")
	h.runUntilCommandExits(t)
	h.cancel()
	h.waitFinished(t, 5*time.Second)

	again := newHarness(t)
	again.wsPath, again.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	again.cfg.LauncherPath, again.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	again.task.Spec.Goal = &v1alpha1.Goal{Text: "first goal"}
	again.start(t)
	waitHealthy(t, again.cfg.Port)
	time.Sleep(200 * time.Millisecond)
	if n := runs(t, dir); n != 1 {
		t.Fatalf("launcher ran %d times for the same goal, want 1", n)
	}
	again.cancel()
	again.waitFinished(t, 5*time.Second)

	next := newHarness(t)
	next.wsPath, next.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	next.cfg.LauncherPath, next.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	next.task.Spec.Goal = &v1alpha1.Goal{Text: "second goal"}
	next.runUntilCommandExits(t)
	if n := runs(t, dir); n != 2 {
		t.Errorf("launcher ran %d times after a new goal, want 2", n)
	}
	if out, _ := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output")); !strings.HasPrefix(string(out), "second goal") {
		t.Errorf("output = %q, want the second goal's", out)
	}
}

// A launcher that exits non-zero does not record launcher-done, so resuming
// the sandbox retries it.
func TestRun_RetriesFailedLauncherOnResume(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "retry goal")
	failOnce := `#!/bin/sh
runs_file="$(dirname "$0")/runs"
echo run >> "$runs_file"
if [ "$(grep -c run "$runs_file")" -eq 1 ]; then
  exit 1
fi
cat "$AX_GOAL_FILE" > "$AX_OUTPUT_FILE"
`
	if err := os.WriteFile(h.cfg.LauncherPath, []byte(failOnce), 0o755); err != nil {
		t.Fatal(err)
	}
	if exit := h.runUntilCommandExits(t); exit.Err == nil {
		t.Fatalf("expected first run to fail, got %+v", exit)
	}
	h.cancel()
	h.waitFinished(t, 5*time.Second)

	retry := newHarness(t)
	retry.wsPath, retry.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	retry.cfg.LauncherPath, retry.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	retry.task.Spec.Goal = &v1alpha1.Goal{Text: "retry goal"}
	if exit := retry.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("expected retry on resume to succeed, got %+v", exit)
	}
	if n := runs(t, dir); n != 2 {
		t.Errorf("launcher ran %d times across failed run and resume, want 2", n)
	}
}

// resume starts a runner on h's workspace and launcher with goal, waits until
// it serves, and stops it.
func resume(t *testing.T, h *harness, goal string) {
	t.Helper()
	again := newHarness(t)
	again.wsPath, again.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	again.cfg.LauncherPath, again.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	again.task.Spec.Goal = &v1alpha1.Goal{Text: goal}
	again.start(t)
	waitHealthy(t, again.cfg.Port)
	time.Sleep(200 * time.Millisecond)
	again.cancel()
	again.waitFinished(t, 5*time.Second)
}

// A launcher-done marker the runner cannot open, such as one owned by the
// user the command ran as, is replaced when the launcher finishes a new goal.
func TestRun_ReplacesUnwritableLauncherDone(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "first goal")
	h.runUntilCommandExits(t)
	h.cancel()
	h.waitFinished(t, 5*time.Second)
	if err := os.Chmod(filepath.Join(h.wsPath, ".ax", "launcher-done"), 0o444); err != nil {
		t.Fatal(err)
	}

	next := newHarness(t)
	next.wsPath, next.task.Spec.Workspace.Path = h.wsPath, h.wsPath
	next.cfg.LauncherPath, next.cfg.RunDir = h.cfg.LauncherPath, h.cfg.RunDir
	next.task.Spec.Goal = &v1alpha1.Goal{Text: "second goal"}
	if exit := next.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	next.cancel()
	next.waitFinished(t, 5*time.Second)

	resume(t, h, "second goal")
	if n := runs(t, dir); n != 2 {
		t.Errorf("launcher ran %d times, want 2: the second goal ran again on resume", n)
	}
}

// A runner that is root without CAP_DAC_OVERRIDE records launcher-done in a
// .ax directory owned by the unprivileged user, and gives the directory back
// (#740). Run it with: docker run --cap-drop DAC_OVERRIDE ... go test ./runner
func TestRun_RecordsLauncherDoneInUserOwnedAxDir(t *testing.T) {
	if os.Geteuid() != 0 || hasCapability(t, 1 /* CAP_DAC_OVERRIDE */) {
		t.Skip("needs root without CAP_DAC_OVERRIDE")
	}
	h := newHarness(t)
	dir := withLauncher(t, h, "one-shot goal")
	axDir := filepath.Join(h.wsPath, ".ax")
	if err := os.MkdirAll(axDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The state after a first run's setup handed .ax to UID 1000: setup has
	// run, and the launcher (root here) has an output file it owns.
	for _, name := range []string{workspace.InitializedMarkerFilename, "output"} {
		if err := os.WriteFile(filepath.Join(axDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(axDir, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	// Give .ax back so that t.TempDir can remove it.
	t.Cleanup(func() { _ = os.Chown(axDir, 0, 0) })

	if exit := h.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	h.cancel()
	h.waitFinished(t, 5*time.Second)
	resume(t, h, "one-shot goal")
	if n := runs(t, dir); n != 1 {
		t.Errorf("launcher ran %d times for the same goal, want 1", n)
	}
	info, err := os.Stat(axDir)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 1000 || st.Gid != 1000 {
		t.Errorf(".ax is owned by %d:%d, want 1000:1000", st.Uid, st.Gid)
	}
}

// hasCapability reports whether the process has capability bit in its
// effective set, read from /proc/self/status.
func hasCapability(t *testing.T, bit uint) bool {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(status), "\n") {
		if hex, ok := strings.CutPrefix(line, "CapEff:"); ok {
			caps, err := strconv.ParseUint(strings.TrimSpace(hex), 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			return caps&(1<<bit) != 0
		}
	}
	return true
}

// When Config.RunDir is empty and DefaultRunDir (/run/ax) cannot be created by
// a non-root user, the runner falls back to a unique temporary directory under
// os.TempDir() instead of a predictable shared path.
func TestRun_FallsBackWhenDefaultRunDirUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to /run/ax")
	}
	tmpBase := t.TempDir()
	t.Setenv("TMPDIR", tmpBase)

	h := newHarness(t)
	withLauncher(t, h, "non-root goal")
	h.cfg.RunDir = ""

	if exit := h.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit with empty RunDir: %+v", exit)
	}
	out, err := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output"))
	if err != nil || !strings.HasPrefix(string(out), "non-root goal") {
		t.Errorf("output = %q (%v), want non-root goal", out, err)
	}
	entries, err := os.ReadDir(tmpBase)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "ax-run-") && e.Name() != "ax-run" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a unique ax-run-* directory under %s, got %v", tmpBase, entries)
	}
}

// Explicit commands also get the contract's variables, so any command can
// write $AX_OUTPUT_FILE; the launcher is not started for them.
func TestRun_CommandsGetContractEnv(t *testing.T) {
	h := newHarness(t)
	dir := withLauncher(t, h, "goal")
	h.task.Spec.Commands = []string{"sh", "-c", `cat "$AX_GOAL_FILE" > "$AX_OUTPUT_FILE"`}

	if exit := h.runUntilCommandExits(t); exit.Err != nil {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	if out, _ := os.ReadFile(filepath.Join(h.wsPath, ".ax", "output")); string(out) != "goal" {
		t.Errorf("output = %q, want the goal", out)
	}
	if n := runs(t, dir); n != 0 {
		t.Errorf("launcher ran %d times for a task with commands, want 0", n)
	}
}

func TestRun_MissingLauncherServes(t *testing.T) {
	h := newHarness(t)
	withLauncher(t, h, "goal")
	h.cfg.LauncherPath = filepath.Join(t.TempDir(), "missing")
	h.start(t)

	waitHealthy(t, h.cfg.Port)
	select {
	case <-h.finished:
		t.Fatalf("Run returned before cancellation: %v", h.err)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestMCPServers(t *testing.T) {
	got := runner.MCPServers(&v1alpha1.MCPConfig{
		Servers: []*v1alpha1.MCPServer{
			{Name: "docs", Endpoint: "https://example.com/mcp"},
			{Name: "git", Command: "git-mcp", Args: []string{"--repo", "."}},
			{Name: "empty"},
		},
		Registries: []*v1alpha1.MCPRegistry{{Servers: []*v1alpha1.MCPServer{
			{Name: "docs", Endpoint: "https://shadowed.example.com/mcp"},
		}}},
	})
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"docs":{"type":"http","url":"https://example.com/mcp"},"git":{"args":["--repo","."],"command":"git-mcp","type":"stdio"}}`
	if string(b) != want {
		t.Errorf("MCPServers = %s, want %s", b, want)
	}
}
