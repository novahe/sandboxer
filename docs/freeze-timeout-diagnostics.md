# Freeze timeout diagnostics

If sandbox-init has not observed `cgroup.events:frozen 1` after its existing
three-second confirmation deadline, it emits a best-effort snapshot of the
freeze cgroup subtree before returning the original error. Successful freezes
perform no extra procfs reads. This does not change the deadline, freezer
control values, recovery policy, or management protocol.

The summary reports the requested freeze value, cgroup events, scanned groups
and tasks, process and D-thread counts, vanished/unreadable entries, truncation,
and scan duration. Two ps-style tables follow: D-state threads first, then
the process tree. Columns are PID, PPID, TID, STAT, THR, D, WCHAN, CGROUP,
NSPID and COMMAND. THR and D are scanned thread and D-thread counts for
the process; STAT is the single state letter from procfs, without ps modifier
flags. COMMAND is the quoted comm name, not the command line. The process
tree uses indentation and a \_ marker in COMMAND. WCHAN is read only for
D threads; process rows reuse it for a D leader, otherwise displaying `-`. A process leader
may be S while a worker is D; the separate thread records expose this case.

IDs are read from sandbox-init's procfs view. NSpid helps match IDs in nested
PID namespaces. Parents outside the selected cgroup subtree are not scanned;
their in-scope children appear as roots. Concurrent exits and cgroup migration
can make the snapshot incomplete; missing leaders are shown with state `?`.

Collection uses only the Go standard library: cgroup.threads identifies tasks,
and /proc/TID/status supplies their identity, state and parent. Only D threads
need an additional /proc/TID/wchan read. No ps subprocess, workload file,
command line, environment, or kernel stack is read.

To bound failure-path overhead, collection stops at 128 cgroups, 2048 unique
task IDs, or 250 ms, checked between reads. Individual reads are size-limited.
Output is capped at 128 blocked-thread/process-tree rows, plus the summary,
table titles/headers and an optional truncation notice. The time budget cannot interrupt a kernel
read or a console write; it is not a hard wall-clock deadline. Limits and
vanished/unreadable counters make incomplete output visible.

Example (abbreviated; common log prefix, CGROUP and NSPID columns omitted):

```text
D-state threads:
    PID    PPID     TID STAT  THR    D WCHAN                            COMMAND
     52      51      55 D      11    8 folio_wait_bit_common            "nfs-writer"
Process tree:
    PID    PPID     TID STAT  THR    D WCHAN                            COMMAND
     51       1      51 S       1    0 -                                "sh"
     52      51      52 S      11    8 -                                \_ "nfs-writer"
```
