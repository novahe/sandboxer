package main

import (
	"bufio"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Diagnostics run only after freeze confirmation times out. Bound both the
// procfs work and console output so a large workload cannot flood the console.
const (
	freezeDiagTasks  = 2048
	freezeDiagGroups = 128
	freezeDiagLines  = 128
	freezeDiagBudget = 250 * time.Millisecond
)

type freezeDiagTask struct {
	pid, tid, ppid            int
	state, name, nspid, group string
}

type freezeDiagProcess struct {
	leader         freezeDiagTask
	tasks, blocked int
}

// dumpCgroupFreezeTree uses the observer's procfs/PID namespace, not the
// workload's scoped cgroup namespace. NSpid helps correlate guest-visible IDs.
// It never forks a command or reads workload files, cmdlines or environments.
// The time budget is checked between reads; it cannot interrupt a procfs read.
func dumpCgroupFreezeTree(root, proc string, printf func(string, ...any)) {
	start := time.Now()
	processes := make(map[int]*freezeDiagProcess)
	seen := make(map[int]bool)
	var blocked []freezeDiagTask
	groups, tasks, unreadable, vanished := 0, 0, 0, 0
	truncated := false
	noteError := func(err error) {
		if os.IsNotExist(err) {
			vanished++
		} else {
			unreadable++
		}
	}
	expired := func() bool { return time.Since(start) >= freezeDiagBudget }
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			noteError(err)
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if groups >= freezeDiagGroups || tasks >= freezeDiagTasks || expired() {
			truncated = true
			return fs.SkipAll
		}
		groups++
		ids, cut, err := readFreezeDiagFile(filepath.Join(path, "cgroup.threads"), 64*1024)
		truncated = truncated || cut
		if err != nil {
			noteError(err)
			return nil
		}
		for _, id := range strings.Fields(ids) {
			if tasks >= freezeDiagTasks || expired() {
				truncated = true
				return fs.SkipAll
			}
			tid, err := strconv.Atoi(id)
			if err != nil || tid <= 0 || seen[tid] {
				continue
			}
			seen[tid] = true
			tasks++
			status, cut, err := readFreezeDiagFile(filepath.Join(proc, id, "status"), 4096)
			truncated = truncated || cut
			if err != nil {
				noteError(err)
				continue
			}
			task, ok := parseFreezeDiagStatus(status)
			if !ok {
				unreadable++
				continue
			}
			task.group = path
			p := processes[task.pid]
			if p == nil {
				p = &freezeDiagProcess{leader: task}
				p.leader.tid = task.pid
				p.leader.state = "?" // the leader may have exited or moved
				processes[task.pid] = p
			}
			if task.tid == task.pid {
				p.leader = task
			}
			p.tasks++
			if task.state == "D" {
				p.blocked++
				blocked = append(blocked, task)
			}
		}
		return nil
	})
	freeze, _, freezeErr := readFreezeDiagFile(filepath.Join(root, "cgroup.freeze"), 64)
	events, _, eventsErr := readFreezeDiagFile(filepath.Join(root, "cgroup.events"), 1024)
	if freezeErr != nil {
		noteError(freezeErr)
	}
	if eventsErr != nil {
		noteError(eventsErr)
	}
	printf("cgroup freeze timeout: root=%q freeze=%q events=%q groups=%d tasks=%d processes=%d D=%d vanished=%d unreadable=%d truncated=%t scan=%s",
		root, strings.TrimSpace(freeze), strings.TrimSpace(events), groups, tasks, len(processes), len(blocked), vanished, unreadable, truncated, time.Since(start))
	if len(processes) == 0 {
		return
	}
	printHeader := func(title string) {
		printf("cgroup freeze timeout: %s", title)
		printf("cgroup freeze timeout: %7s %7s %7s %-4s %4s %4s %-32s %-32s %-16s %s",
			"PID", "PPID", "TID", "STAT", "THR", "D", "WCHAN", "CGROUP", "NSPID", "COMMAND")
	}
	lines := 0
	wchans := make(map[int]string)
	if len(blocked) > 0 {
		printHeader("D-state threads:")
	}
	// Print D threads first: a Go process leader may be S while workers are D,
	// and the process tree may be too large to print in full.
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].tid < blocked[j].tid })
	for _, task := range blocked {
		if lines >= freezeDiagLines {
			break
		}
		wchan, _, err := readFreezeDiagFile(filepath.Join(proc, strconv.Itoa(task.tid), "wchan"), 256)
		if err != nil {
			wchan = "unavailable"
		}
		wchans[task.tid] = strings.TrimSpace(wchan)
		p := processes[task.pid]
		printf("cgroup freeze timeout: %7d %7d %7d %-4s %4d %4d %-32s %-32q %-16q %s",
			task.pid, task.ppid, task.tid, task.state, p.tasks, p.blocked, wchans[task.tid], task.group, task.nspid, strconv.Quote(task.name))
		lines++
	}
	printHeader("Process tree:")
	children := make(map[int][]int)
	var roots []int
	for pid, p := range processes {
		if _, ok := processes[p.leader.ppid]; ok && p.leader.ppid != pid {
			children[p.leader.ppid] = append(children[p.leader.ppid], pid)
		} else {
			roots = append(roots, pid)
		}
	}
	sort.Ints(roots)
	for _, ids := range children {
		sort.Ints(ids)
	}
	printed := make(map[int]bool)
	var printTree func(int, int)
	printTree = func(pid, depth int) {
		if printed[pid] || lines >= freezeDiagLines {
			return
		}
		printed[pid] = true
		p := processes[pid]
		wchan := wchans[pid]
		if wchan == "" {
			wchan = "-"
		}
		command := strconv.Quote(p.leader.name)
		if depth > 0 {
			command = strings.Repeat("  ", min(depth-1, 15)) + `\_ ` + command
		}
		printf("cgroup freeze timeout: %7d %7d %7d %-4s %4d %4d %-32s %-32q %-16q %s",
			pid, p.leader.ppid, pid, p.leader.state, p.tasks, p.blocked, wchan, p.leader.group, p.leader.nspid, command)
		lines++
		for _, child := range children[pid] {
			printTree(child, depth+1)
		}
	}
	for _, pid := range roots {
		printTree(pid, 0)
	}
	// PID reuse/reparenting during collection can produce an inconsistent
	// parent graph. Still report disconnected cycles rather than lose tasks.
	remaining := make([]int, 0, len(processes))
	for pid := range processes {
		if !printed[pid] {
			remaining = append(remaining, pid)
		}
	}
	sort.Ints(remaining)
	for _, pid := range remaining {
		printTree(pid, 0)
	}
	if lines < len(blocked)+len(processes) {
		printf("cgroup freeze timeout: output truncated at %d task/tree lines", freezeDiagLines)
	}
}

func readFreezeDiagFile(path string, limit int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	cut := int64(len(b)) > limit
	if cut {
		b = b[:limit]
	}
	return string(b), cut, err
}

func parseFreezeDiagStatus(status string) (freezeDiagTask, bool) {
	var t freezeDiagTask
	scanner := bufio.NewScanner(strings.NewReader(status))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			t.name = value
		case "State":
			if fields := strings.Fields(value); len(fields) > 0 {
				t.state = fields[0]
			}
		case "Tgid":
			t.pid, _ = strconv.Atoi(value)
		case "Pid":
			t.tid, _ = strconv.Atoi(value)
		case "PPid":
			t.ppid, _ = strconv.Atoi(value)
		case "NSpid":
			t.nspid = strings.Join(strings.Fields(value), " ")
		}
	}
	return t, t.pid > 0 && t.tid > 0 && len(t.state) == 1
}
