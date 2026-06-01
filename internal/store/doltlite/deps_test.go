package doltlite_test

import (
	"context"
	"testing"
	"time"

	"autosk/internal/store"
)

func TestBlockerTerminalSemanticsForReadyAndIsBlocked(t *testing.T) {
	cases := []struct {
		name        string
		status      store.Status
		wantBlocked bool
		wantReady   bool
	}{
		{name: "new", status: store.StatusNew, wantBlocked: true},
		{name: "work", status: store.StatusWork, wantBlocked: true},
		{name: "human", status: store.StatusHuman, wantBlocked: true},
		{name: "done", status: store.StatusDone, wantReady: true},
		{name: "cancel", status: store.StatusCancel, wantReady: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := freshStore(t)

			target, err := s.CreateTask(ctx, store.Task{
				Title:    "target",
				Status:   store.StatusNew,
				Priority: 2,
			})
			if err != nil {
				t.Fatalf("CreateTask target: %v", err)
			}
			blockerID := createBlockerWithStatus(ctx, t, s, tc.status)
			if err := s.Block(ctx, target.ID, blockerID); err != nil {
				t.Fatalf("Block: %v", err)
			}

			blocked, err := s.IsBlocked(ctx, target.ID)
			if err != nil {
				t.Fatalf("IsBlocked: %v", err)
			}
			if blocked != tc.wantBlocked {
				t.Fatalf("IsBlocked = %v, want %v", blocked, tc.wantBlocked)
			}

			ready, err := s.Ready(ctx, 0)
			if err != nil {
				t.Fatalf("Ready: %v", err)
			}
			found := false
			for _, task := range ready {
				if task.ID == target.ID {
					found = true
				}
			}
			if found != tc.wantReady {
				t.Fatalf("target ready = %v, want %v (ready=%v)", found, tc.wantReady, ready)
			}
		})
	}
}

func createBlockerWithStatus(ctx context.Context, t *testing.T, s interface {
	CreateTask(context.Context, store.Task) (store.Task, error)
	ExecRaw(context.Context, string, ...any) (store.Result, error)
}, status store.Status) string {
	t.Helper()
	if status == store.StatusWork {
		const (
			agentID = "ag-work"
			wfID    = "wf-work"
			stepID  = "st-work"
		)
		now := time.Now().UTC().Unix()
		if _, err := s.ExecRaw(ctx, `INSERT INTO agents(id, name, is_human, created_at) VALUES (?, ?, 0, ?)`, agentID, "work-agent", now); err != nil {
			t.Fatalf("insert agent: %v", err)
		}
		if _, err := s.ExecRaw(ctx, `INSERT INTO workflows(id, name, first_step_id, created_at) VALUES (?, ?, ?, ?)`, wfID, "work-flow", stepID, now); err != nil {
			t.Fatalf("insert workflow: %v", err)
		}
		if _, err := s.ExecRaw(ctx, `INSERT INTO steps(id, workflow_id, name, agent_id, seq) VALUES (?, ?, ?, ?, 0)`, stepID, wfID, "dev", agentID); err != nil {
			t.Fatalf("insert step: %v", err)
		}
		blocker, err := s.CreateTask(ctx, store.Task{
			Title:         "blocker work",
			Status:        store.StatusWork,
			Priority:      2,
			WorkflowID:    wfID,
			CurrentStepID: stepID,
		})
		if err != nil {
			t.Fatalf("CreateTask work blocker: %v", err)
		}
		return blocker.ID
	}
	blocker, err := s.CreateTask(ctx, store.Task{
		Title:    "blocker " + string(status),
		Status:   status,
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("CreateTask blocker: %v", err)
	}
	return blocker.ID
}
