package store

import (
	"sync"
	"sync/atomic"
	"time"
)

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
	StatusDuplicate  = "duplicate"
	StatusFailed     = "failed"

	TaskProcessing = "processing"
	TaskDone       = "done"
)

type FileItem struct {
	OrigName string
	Size     int64
	Path     string
	Status   string
	MD5      string
	OutName  string
	OutSize  int64
	Reason   string
}

type TaskSummary struct {
	ID          string
	CreatedAt   time.Time
	Concurrency int
	Quality     int
	Files       int
	Done        int
	Elapsed     time.Duration
}

// Task 是处理期的可变实体：done 计数用 atomic，其余字段由 mu 保护；
// 读方一律经 GetTask 返回的快照（TaskView）访问，避免读到半写状态。
type Task struct {
	ID          string
	CreatedAt   time.Time
	Concurrency int
	Quality     int

	mu      sync.Mutex
	Status  string
	Files   []*FileItem
	Elapsed time.Duration

	done atomic.Int64
}

func (t *Task) Done() int64 { return t.done.Load() }

func (t *Task) AddDone() { t.done.Add(1) }

func (t *Task) Finish(elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = TaskDone
	t.Elapsed = elapsed
}

// Lock / Unlock 暴露给 service：worker 更新 FileItem 字段时持同一把锁。
func (t *Task) Lock() { t.mu.Lock() }

func (t *Task) Unlock() { t.mu.Unlock() }

type TaskView struct {
	ID          string
	CreatedAt   time.Time
	Concurrency int
	Quality     int
	Status      string
	Done        int64
	Elapsed     time.Duration
	Files       []FileItem
}

func (t *Task) snapshot() TaskView {
	files := make([]FileItem, len(t.Files))
	for i, f := range t.Files {
		files[i] = *f
	}
	return TaskView{
		ID:          t.ID,
		CreatedAt:   t.CreatedAt,
		Concurrency: t.Concurrency,
		Quality:     t.Quality,
		Status:      t.Status,
		Done:        t.done.Load(),
		Elapsed:     t.Elapsed,
		Files:       files,
	}
}

type Store struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

func New() *Store {
	return &Store{tasks: make(map[string]*Task)}
}

func (s *Store) CreateTask(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[t.ID] = t
}

func (s *Store) GetTask(id string) (TaskView, bool) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	s.mu.Unlock()
	if !ok {
		return TaskView{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot(), true
}

// GetRef 供 service 在任务生命周期内持有可变实体并派发 worker；
// 只读方一律走 GetTask 快照，不得使用本方法。
func (s *Store) GetRef(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id]
}

func (s *Store) ListHistory() []TaskSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskSummary, 0, len(s.tasks))
	for _, t := range s.tasks {
		t.mu.Lock()
		out = append(out, TaskSummary{
			ID:          t.ID,
			CreatedAt:   t.CreatedAt,
			Concurrency: t.Concurrency,
			Quality:     t.Quality,
			Files:       len(t.Files),
			Done:        int(t.done.Load()),
			Elapsed:     t.Elapsed,
		})
		t.mu.Unlock()
	}
	return out
}
