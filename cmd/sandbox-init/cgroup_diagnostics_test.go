package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreezeDiagnosticsTreeAndBlockedWorker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sandbox")
	proc := t.TempDir()
	put := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	status := func(pid, tid, parent int, state, name string) {
		put(filepath.Join(proc, fmt.Sprint(tid), "status"), fmt.Sprintf("Name:\t%s\nState:\t%s (sleeping)\nTgid:\t%d\nPid:\t%d\nPPid:\t%d\nNSpid:\t%d\t%d\n", name, state, pid, tid, parent, tid, tid-9))
		put(filepath.Join(proc, fmt.Sprint(tid), "wchan"), "rpc_wait_bit_killable\n")
	}
	put(filepath.Join(root, "cgroup.freeze"), "1\n")
	put(filepath.Join(root, "cgroup.events"), "populated 1\nfrozen 0\n")
	put(filepath.Join(root, "cgroup.threads"), "10\n")
	put(filepath.Join(root, "user", "cgroup.threads"), "20\n21\n999\n")
	status(10, 10, 1, "S", "envd")
	status(20, 20, 10, "S", "writer")
	status(20, 21, 10, "D", "writer worker")
	status(88, 88, 1, "D", "outside")
	var lines []string
	dumpCgroupFreezeTree(root, proc, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	out := strings.Join(lines, "\n")
	for _, want := range []string{"processes=2 D=1 vanished=1", "D-state threads:", "Process tree:", "rpc_wait_bit_killable", `\_ "writer"`, `"21 12"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "outside") {
		t.Fatal("escaped target cgroup subtree")
	}
	compact := strings.Join(strings.Fields(out), " ")
	for _, row := range []string{"20 10 21 D 2 1 rpc_wait_bit_killable", "10 1 10 S 1 0 -", "20 10 20 S 2 1 -"} {
		if !strings.Contains(compact, row) {
			t.Errorf("missing table row %q in %s", row, out)
		}
	}
	t.Log("\n" + out)
	if strings.Index(out, "D-state threads:") > strings.Index(out, "Process tree:") {
		t.Fatal("blocked threads should be printed first")
	}
}

func TestFreezeDiagnosticsOutputLimitAndDThreadPriority(t *testing.T) {
	root := t.TempDir()
	proc := t.TempDir()
	var ids strings.Builder
	for i := 1; i <= freezeDiagLines+5; i++ {
		fmt.Fprintln(&ids, i)
		dir := filepath.Join(proc, fmt.Sprint(i))
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		state := "S"
		if i == freezeDiagLines+5 {
			state = "D"
		}
		data := fmt.Sprintf("Name:\ttask\nState:\t%s\nTgid:\t%d\nPid:\t%d\nPPid:\t0\n", state, i, i)
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.threads"), []byte(ids.String()), 0644); err != nil {
		t.Fatal(err)
	}
	var lines []string
	dumpCgroupFreezeTree(root, proc, func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if len(lines) != freezeDiagLines+6 {
		t.Fatalf("expected bounded output, got %d lines", len(lines))
	}
	first := strings.Join(strings.Fields(lines[3]), " ")
	if !strings.Contains(first, "133 0 133 D 1 1") {
		t.Fatalf("lost D task: %v", lines[:4])
	}
	if !strings.Contains(lines[len(lines)-1], "output truncated") {
		t.Fatal("missing truncation notice")
	}
}

func TestParseFreezeDiagStatus(t *testing.T) {
	for _, s := range []string{"", "Pid: 1\nTgid: 1\n", "Pid: invalid\nTgid: 1\nState: D\n"} {
		if _, ok := parseFreezeDiagStatus(s); ok {
			t.Fatalf("accepted malformed status %q", s)
		}
	}
	task, ok := parseFreezeDiagStatus("Name:\tname with spaces\nState:\tD (disk sleep)\nTgid:\t2\nPid:\t3\nPPid:\t1\n")
	if !ok || task.name != "name with spaces" || task.state != "D" || task.pid != 2 || task.tid != 3 {
		t.Fatalf("bad parse: %+v", task)
	}
}

func TestFreezeDiagnosticsMissingRoot(t *testing.T) {
	var lines []string
	dumpCgroupFreezeTree(filepath.Join(t.TempDir(), "gone"), t.TempDir(), func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	if len(lines) != 1 || !strings.Contains(lines[0], "processes=0") || !strings.Contains(lines[0], "vanished=3") {
		t.Fatalf("unexpected missing-root diagnostics: %v", lines)
	}
}
