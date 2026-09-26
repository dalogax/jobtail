// Package e2e black-box tests the built jobtail binary the way a user
// would: as a subprocess, against a scratch data dir, asserting on its
// stdout/exit code and on the SQLite state it leaves behind.
package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

// binary builds cmd/jobtail once per test process and returns its path.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		// A process-lifetime dir (not t.TempDir()) so the binary survives
		// across every test in this package, built exactly once.
		dir := filepath.Join(os.TempDir(), "jobtail-e2e-bin")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "jobtail")
		cmd := exec.Command("go", "build", "-o", binPath, "github.com/jarvis0064/jobtail/cmd/jobtail")
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = errCombined(err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build jobtail: %v", buildErr)
	}
	return binPath
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Dir(wd) // e2e/ -> repo root
}

func errCombined(err error, out []byte) error {
	return &buildError{err: err, out: out}
}

type buildError struct {
	err error
	out []byte
}

func (b *buildError) Error() string { return b.err.Error() + ": " + string(b.out) }

// env is a fresh, isolated jobtail environment: its own data dir, so tests
// never touch the real ~/.local/share/jobtail on this machine.
type env struct {
	t       *testing.T
	bin     string
	dataDir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return &env{t: t, bin: binary(t), dataDir: t.TempDir()}
}

// run invokes `jobtail <args...>` and returns combined stdout+stderr. It
// fails the test if the process errors, unless the caller wants to inspect
// a failure itself (use runAllowFail for that).
func (e *env) run(args ...string) string {
	e.t.Helper()
	out, err := e.runAllowFail(args...)
	if err != nil {
		e.t.Fatalf("jobtail %v: %v\noutput:\n%s", args, err, out)
	}
	return out
}

func (e *env) runAllowFail(args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_DISABLE_NOTIFY=1",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// start is like runAllowFail but doesn't wait — for tests that need to race
// two invocations against each other (overlap handling).
func (e *env) start(args ...string) (*exec.Cmd, *bytes.Buffer) {
	e.t.Helper()
	cmd := exec.Command(e.bin, args...)
	cmd.Env = append(os.Environ(),
		"JOBTAIL_DATA_DIR="+e.dataDir,
		"JOBTAIL_DISABLE_NOTIFY=1",
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		e.t.Fatalf("start jobtail %v: %v", args, err)
	}
	return cmd, &buf
}
