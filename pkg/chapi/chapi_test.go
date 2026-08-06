package chapi

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// TestWaitRestored drives chapi.WaitRestored with fake CH event-monitor
// streams (whitespace-separated JSON objects, as CH's monitor thread writes).
// Each case runs inside a testing/synctest bubble, so the deadline/cancel
// waits resolve in fake time (no real sleeping).
func TestWaitRestored(t *testing.T) {
	restored := `{"timestamp":{"secs":0,"nanos":2},"source":"vm","event":"restored","properties":null}`
	restoring := `{"timestamp":{"secs":0,"nanos":1},"source":"vm","event":"restoring","properties":null}`
	join := func(events ...string) string {
		var b strings.Builder
		for _, e := range events {
			b.WriteString(e)
			b.WriteString("\n\n")
		}
		return b.String()
	}

	cases := []struct {
		name        string
		stream      string
		keepOpen    bool          // leave the write-end open so Decode blocks (exercises the timeout path)
		deadline    time.Duration
		cancelAfter time.Duration // if >0, cancel ctx after this delay (exercises the ctx path)
		wantErr     bool
	}{
		{name: "restoring-then-restored", stream: join(restoring, restored), deadline: time.Second},
		{name: "restored-only", stream: join(restored), deadline: time.Second},
		{name: "restoring-then-EOF", stream: join(restoring), deadline: time.Second, wantErr: true},
		{name: "deadline-blocked-no-stream", stream: "", keepOpen: true, deadline: 50 * time.Millisecond, wantErr: true},
		{name: "ctx-cancel-blocked", stream: "", keepOpen: true, deadline: 2 * time.Second, cancelAfter: 50 * time.Millisecond, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				r, w := io.Pipe()
				go func() {
					_, _ = io.WriteString(w, tc.stream)
					if !tc.keepOpen {
						_ = w.Close()
					}
				}()
				if tc.cancelAfter > 0 {
					go func() {
						time.Sleep(tc.cancelAfter)
						cancel()
					}()
				}

				err := WaitRestored(ctx, r, tc.deadline)
				if tc.keepOpen {
					_ = w.Close() // release the still-open write-end
				}
				if tc.wantErr && err == nil {
					t.Fatal("WaitRestored returned nil, want error")
				}
				if !tc.wantErr && err != nil {
					t.Fatalf("WaitRestored returned %v, want nil", err)
				}
			})
		})
	}
}
