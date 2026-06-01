package tui

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"autosk/internal/daemon/api"
	"autosk/internal/lazy/datasource"
)

// debounceFakeDS is a refreshFakeDS specialisation that records every
// StreamLive call so the debounce tests can assert on the call count
// + the final job id.
type debounceFakeDS struct {
	refreshFakeDS
	calls atomic.Int32
	last  atomic.Value // string
}

func (f *debounceFakeDS) StreamLive(_ context.Context, jobID string) (*datasource.LiveHandle, error) {
	f.calls.Add(1)
	f.last.Store(jobID)
	// Return a handle whose channel never produces (the pump loop is
	// irrelevant for the debounce tests).
	ch := make(chan datasource.LiveEvent)
	return datasource.NewLiveHandle(ch, func() error { close(ch); return nil }), nil
}

func (*debounceFakeDS) SyncWorkflows(_ context.Context, _, _ bool) (datasource.SyncReport, error) { return datasource.SyncReport{}, nil }
func newDebounceGui(t *testing.T) (*Gui, *debounceFakeDS) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ds := &debounceFakeDS{}
	gu := &Gui{
		st:  newState(),
		ds:  ds,
		ctx: ctx,
	}
	return gu, ds
}

// TestScheduleJobLive_DebounceCoalesces pins acceptance criterion 10:
// 20 calls in <1s collapse into ONE StreamLive invocation, made
// jobLiveDebounce after the last call. Test scope: the real
// scheduleJobLive (using the real timer) — we shorten the wait by
// just sleeping past the debounce window.
func TestScheduleJobLive_DebounceCoalesces(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce test sleeps for 2s+; -short skips")
	}
	gu, ds := newDebounceGui(t)
	// Seed jobs so the selectedJob check inside openJobLive sees a
	// running job.
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{{JobResponse: api.JobResponse{
			JobID:  "job-final",
			Status: "running",
		}}}
		gu.st.jobCursor = 0
	})

	for i := 0; i < 20; i++ {
		gu.scheduleJobLive("job-final", true)
		time.Sleep(5 * time.Millisecond) // 20 * 5ms = 100ms total burst
	}
	// Burst total = 100ms. Wait the debounce window + slack.
	time.Sleep(jobLiveDebounce + 200*time.Millisecond)

	if got := ds.calls.Load(); got != 1 {
		t.Errorf("StreamLive calls = %d, want exactly 1 (20 schedules in 100ms must collapse)", got)
	}
	if got, _ := ds.last.Load().(string); got != "job-final" {
		t.Errorf("StreamLive last jobID = %q, want job-final", got)
	}
	gu.stopJobLive()
}

// TestScheduleJobLive_CursorMoveCancelsPrevious: scheduling for jobA
// then jobB within the debounce window must open ONLY jobB.
func TestScheduleJobLive_CursorMoveCancelsPrevious(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce test sleeps for 2s+; -short skips")
	}
	gu, ds := newDebounceGui(t)
	gu.st.withLock(func() {
		gu.st.jobs = []datasource.Job{
			{JobResponse: api.JobResponse{JobID: "job-A", Status: "running"}},
			{JobResponse: api.JobResponse{JobID: "job-B", Status: "running"}},
		}
		gu.st.jobCursor = 1
	})
	// Schedule jobA — within the window, schedule jobB. Only jobB
	// should win.
	gu.scheduleJobLive("job-A", true)
	time.Sleep(100 * time.Millisecond)
	gu.scheduleJobLive("job-B", true)
	time.Sleep(jobLiveDebounce + 200*time.Millisecond)

	if got := ds.calls.Load(); got != 1 {
		t.Errorf("StreamLive calls = %d, want 1", got)
	}
	if got, _ := ds.last.Load().(string); got != "job-B" {
		t.Errorf("StreamLive last jobID = %q, want job-B (jobA was superseded)", got)
	}
	gu.stopJobLive()
}

// TestStopJobLive_Idempotent: calling stopJobLive when nothing is
// active is a no-op (no panic, no spurious StreamLive call).
func TestStopJobLive_Idempotent(t *testing.T) {
	gu, _ := newDebounceGui(t)
	// First call on an empty state.
	gu.stopJobLive()
	// Second call still empty.
	gu.stopJobLive()
	// And once more.
	gu.stopJobLive()
}

// TestScheduleJobLive_TerminalJobDoesNotOpen: running=false short-
// circuits without opening — and tears down any prior active stream.
func TestScheduleJobLive_TerminalJobDoesNotOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("debounce test sleeps; -short skips")
	}
	gu, ds := newDebounceGui(t)
	// Pretend the cursor never points at a running job.
	gu.scheduleJobLive("job-done", false)
	// Even after a long wait, no StreamLive call.
	time.Sleep(jobLiveDebounce + 200*time.Millisecond)
	if got := ds.calls.Load(); got != 0 {
		t.Errorf("StreamLive calls = %d, want 0 for !running", got)
	}
}
