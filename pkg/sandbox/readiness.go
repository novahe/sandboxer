package sandbox

import "sync"

// ReadinessTiming is the sandbox-ctl-side portion of the launch timeline.
// Unix nanoseconds keep the wire compact and let node-ctl correlate these
// points with its own wall clock without relying on process-relative clocks.
// Zero fields mean that the phase does not exist on this boot path (for
// example, CH resume and restore ACK are absent on a cold image boot).
type ReadinessTiming struct {
	Version               int   `json:"version"`
	SandboxCtlStartUnixNs int64 `json:"sandbox_ctl_start_unix_ns"`
	CHStartUnixNs         int64 `json:"ch_start_unix_ns,omitempty"`
	CHResumeDoneUnixNs    int64 `json:"ch_resume_done_unix_ns,omitempty"`
	SandboxInitAckUnixNs  int64 `json:"sandbox_init_ack_unix_ns,omitempty"`
}

// ReadinessEvent is one startup milestone emitted by sandbox-ctl run.
type ReadinessEvent string

const (
	// ReadinessControlReady means this run's host-local ctl.sock is listening.
	ReadinessControlReady ReadinessEvent = "control_ready"
	// ReadinessReady means the run-specific cold or restore readiness barrier
	// has completed.
	ReadinessReady ReadinessEvent = "ready"
)

// ReadinessNotify observes startup milestones for one sandbox run. A nil
// callback disables readiness notification.
type ReadinessNotify func(ReadinessEvent)

// readinessEmitter centralizes the per-run ordering and once semantics. The
// CLI-owned fd writer independently validates the same invariant at its trust
// boundary, but callers of ServeAndWait cannot make ready overtake ctl.Listen.
type readinessEmitter struct {
	mu           sync.Mutex
	notify       ReadinessNotify
	controlReady bool
	ready        bool
}

func newReadinessEmitter(notify ReadinessNotify) *readinessEmitter {
	return &readinessEmitter{notify: notify}
}

func (e *readinessEmitter) notifyControlReady() {
	if e == nil || e.notify == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.controlReady {
		return
	}
	e.notify(ReadinessControlReady)
	e.controlReady = true
}

func (e *readinessEmitter) notifyReady() {
	if e == nil || e.notify == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.controlReady || e.ready {
		return
	}
	e.notify(ReadinessReady)
	e.ready = true
}
