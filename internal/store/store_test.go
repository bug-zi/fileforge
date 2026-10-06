package store

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCreateAndGetTask(t *testing.T) {
	s := New()
	task := &Task{
		ID:          "abc123",
		CreatedAt:   time.Now(),
		Concurrency: 4,
		Status:      TaskProcessing,
		Files: []*FileItem{
			{OrigName: "a.jpg", Status: StatusPending},
			{OrigName: "b.png", Status: StatusPending},
		},
	}
	s.CreateTask(task)

	got, ok := s.GetTask("abc123")
	if !ok {
		t.Fatal("GetTask returned false for existing task")
	}
	if got.ID != "abc123" {
		t.Errorf("ID = %q, want %q", got.ID, "abc123")
	}
	if got.Concurrency != 4 {
		t.Errorf("Concurrency = %d, want 4", got.Concurrency)
	}
	if len(got.Files) != 2 {
		t.Errorf("len(Files) = %d, want 2", len(got.Files))
	}
	if got.Files[1].OrigName != "b.png" {
		t.Errorf("Files[1].OrigName = %q, want %q", got.Files[1].OrigName, "b.png")
	}
}

func TestGetUnknownTask(t *testing.T) {
	s := New()
	if _, ok := s.GetTask("nope"); ok {
		t.Fatal("GetTask returned true for unknown task")
	}
}

func TestSnapshotIsCopy(t *testing.T) {
	s := New()
	item := &FileItem{OrigName: "a.jpg", Status: StatusPending}
	s.CreateTask(&Task{ID: "t1", Files: []*FileItem{item}})

	view, ok := s.GetTask("t1")
	if !ok {
		t.Fatal("task not found")
	}

	item.Status = StatusDone
	item.MD5 = "deadbeef"

	if view.Files[0].Status != StatusPending {
		t.Errorf("snapshot FileItem mutated with original: Status = %q, want %q", view.Files[0].Status, StatusPending)
	}
	if view.Files[0].MD5 != "" {
		t.Errorf("snapshot FileItem mutated with original: MD5 = %q, want empty", view.Files[0].MD5)
	}
}

func TestDoneCounterAndFinish(t *testing.T) {
	s := New()
	task := &Task{ID: "t2", Status: TaskProcessing}
	s.CreateTask(task)

	task.AddDone()
	task.AddDone()

	got, _ := s.GetTask("t2")
	if got.Done != 2 {
		t.Errorf("Done = %d, want 2", got.Done)
	}

	elapsed := 1500 * time.Millisecond
	task.Finish(elapsed)

	got, _ = s.GetTask("t2")
	if got.Status != TaskDone {
		t.Errorf("Status = %q, want %q", got.Status, TaskDone)
	}
	if got.Elapsed != elapsed {
		t.Errorf("Elapsed = %v, want %v", got.Elapsed, elapsed)
	}
}

func TestListHistory(t *testing.T) {
	s := New()
	s.CreateTask(&Task{ID: "h1", Concurrency: 1, Files: []*FileItem{{}, {}}})
	s.CreateTask(&Task{ID: "h2", Concurrency: 16, Files: []*FileItem{{}}})

	hist := s.ListHistory()
	if len(hist) != 2 {
		t.Fatalf("len(history) = %d, want 2", len(hist))
	}
	byID := map[string]TaskSummary{}
	for _, h := range hist {
		byID[h.ID] = h
	}
	if h := byID["h1"]; h.Files != 2 || h.Concurrency != 1 {
		t.Errorf("h1 summary = %+v, want Files=2 Concurrency=1", h)
	}
	if h := byID["h2"]; h.Files != 1 || h.Concurrency != 16 {
		t.Errorf("h2 summary = %+v, want Files=1 Concurrency=16", h)
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				id := fmt.Sprintf("t-%d-%d", g, i)
				s.CreateTask(&Task{ID: id, Status: TaskProcessing})
				if _, ok := s.GetTask(id); !ok {
					t.Errorf("task %q not found right after create", id)
				}
				s.ListHistory()
			}
		}(g)
	}
	wg.Wait()
}
