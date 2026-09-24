// SPDX-FileCopyrightText: 2026 Nextcloud contributors
// SPDX-License-Identifier: AGPL-3.0-or-later

package pool

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// occ refuses to run outside the Nextcloud root, exactly like console.php:
// "This script can be run from the Nextcloud root directory only."
const cwdSensitiveOcc = `#!/bin/sh
if [ ! -f "./occ" ]; then
  echo "This script can be run from the Nextcloud root directory only." >&2
  exit 1
fi
sleep 30
`

// newFakeNextcloud builds a directory that looks like a Nextcloud root and
// moves the process somewhere else, mimicking a service started from "/".
func newFakeNextcloud(t *testing.T) (root, script string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "occ"), []byte("marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	script = filepath.Join(root, "occ.sh")
	if err := os.WriteFile(script, []byte(cwdSensitiveOcc), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	return root, script
}

// Workers must be launched from the Nextcloud root. Without it occ exits
// immediately and the supervisor respawns it forever without draining anything.
func TestWorkerRunsFromNextcloudRoot(t *testing.T) {
	root, script := newFakeNextcloud(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := New([]string{"/bin/sh", script}, root, 0, nil, logger)
	t.Cleanup(p.Shutdown)

	p.Reconcile(context.Background(), map[string]int{"core:text2text": 1})
	time.Sleep(600 * time.Millisecond)

	if got := p.Running()["core:text2text"]; got != 1 {
		t.Fatalf("worker did not survive: running=%v", p.Running())
	}
}

// When a worker does fail to start, its stderr must reach the log. A bare
// "exit status 1" leaves an operator with nothing to act on.
func TestWorkerFailureReportsStderr(t *testing.T) {
	_, script := newFakeNextcloud(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := New([]string{"/bin/sh", script}, "", 0, nil, logger)

	p.Reconcile(context.Background(), map[string]int{"core:text2text": 1})
	time.Sleep(600 * time.Millisecond)
	p.Shutdown()

	if !strings.Contains(logs.String(), "Nextcloud root directory only") {
		t.Errorf("worker stderr missing from logs:\n%s", logs.String())
	}
}

func TestTailBufferRetainsLastBytes(t *testing.T) {
	single := &tailBuffer{limit: 10}
	if _, err := single.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := single.String(); got != "6789abcdef" {
		t.Errorf("oversized write: got %q, want %q", got, "6789abcdef")
	}

	incremental := &tailBuffer{limit: 10}
	for _, chunk := range []string{"aaaa", "bbbb", "cccc"} {
		if _, err := incremental.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got := incremental.String(); got != "aabbbbcccc" {
		t.Errorf("incremental writes: got %q, want %q", got, "aabbbbcccc")
	}
}
