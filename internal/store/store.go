package store

import (
	"sync"        //互斥锁、WaitGroup、Once 等同步原语的包。本文件只用了它的`sync.Mutex`
	"sync/atomic" // 原子操作包，用于并发安全的整数操作。本文件只用了它的`atomic.AddInt64`
	"time"        // 时间包，用于处理时间相关的操作。本文件只用了它的`time.Now`
)

// 文件五态：pending → processing → done/duplicate/failed
const (
	StatusPending    = "pending"    // 待处理
	StatusProcessing = "processing" // 某个 worker 正在处理
	StatusDone       = "done"       // 压缩成功，进 zip
	StatusDuplicate  = "duplicate"  // MD5 判重命中，跳过处理（也计入 done）
	StatusFailed     = "failed"     // 处理失败，不进 zip
)

// 任务三态：ready → processing → done
const (
	TaskReady      = "ready" // 已上传待确认（v0.3 分步处理）
	TaskProcessing = "processing"
	TaskDone       = "done"
)

// FileItem 记录任务内单个文件的完整生命周期数据。
type FileItem struct {
	OrigName string // 上传时的原始文件名
	Size     int64  // 原始文件字节数
	Path     string // 落盘路径 uploads/{taskID}/
	Status   string // 文件五态之一
	MD5      string // 原始内容摘要，判重依据
	OutName  string // 统一输出名 photo_NNN.jpg
	OutSize  int64  // 压缩后字节数
	Reason   string // duplicate/failed 时的原因说明
}

// TaskSummary 是历史列表用的轻量摘要：只含统计字段，不含文件明细。
type TaskSummary struct {
	ID          string        // 任务 ID
	CreatedAt   time.Time     // 创建时间
	Concurrency int           // 用户指定的并发数
	Quality     int           // JPEG 质量
	Status      string        // 任务状态（ready/processing/done）
	Files       int           // 文件总数
	Done        int           // 已完成文件数
	Elapsed     time.Duration // 处理耗时
}

// Task 是处理期的可变实体：done 计数用 atomic，其余字段由 mu 保护；
// 读方一律经 GetTask 返回的快照（TaskView）访问，避免读到半写状态。
type Task struct {
	ID          string
	CreatedAt   time.Time
	Concurrency int
	Quality     int
	DirName     string // outputs/ 下的时间命名目录（创建时定，不可变）

	// mu 保护下面三个复合状态字段：快照需要 Status/Files/Elapsed 三者一致，
	// 绝不允许读方看到「Status=done 但 Elapsed 还是 0」的半写状态，必须用互斥锁。
	mu      sync.Mutex
	Status  string
	Files   []*FileItem
	Elapsed time.Duration

	// done 用 atomic 而非 mu：它是孤立的单变量计数，无复合一致性要求，
	// atomic.Add/Load 是无锁 CPU 指令，比抢互斥锁便宜得多。
	// 口诀：单变量无临界区→atomic；复合状态需一致快照→Mutex。
	done atomic.Int64
}

func (t *Task) Done() int64 { return t.done.Load() } // 返回一个`int64` ，就是当前已完成的文件数。

func (t *Task) AddDone() { t.done.Add(1) } // 增加已完成的文件数。

// Finish 收尾：同时写 Status 和 Elapsed 是复合操作，必须持 t.mu 保持一致。
// 把任务从「处理中」钉死为「已完成」，同时记录总耗时。
func (t *Task) Finish(elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock() // 确保在函数结束时解锁锁，避免死锁。
	t.Status = TaskDone // 设置任务状态为 done。前端显示“已完成”，可以下载 zip。
	t.Elapsed = elapsed // 设置任务耗时为 elapsed。
}

// Lock / Unlock 暴露给 service：worker 更新 FileItem 字段时持同一把锁。
func (t *Task) Lock() { t.mu.Lock() }

func (t *Task) Unlock() { t.mu.Unlock() }

// TaskView 是对外只读的值拷贝视图：读方（HTTP handler）拿走后可随意持有，
// 不受后续写入影响、无需持锁——代价是拷贝一次内存。
type TaskView struct {
	ID          string        // 任务 ID
	CreatedAt   time.Time     // 创建时间
	Concurrency int           // 并发数
	Quality     int           // JPEG 质量
	DirName     string        // outputs/ 下的时间命名目录（创建时定，不可变）
	Status      string        // 任务状态
	Done        int64         // 已完成文件数（从 atomic 读出的快照值）
	Elapsed     time.Duration // 任务耗时
	Files       []FileItem    // 文件列表（值拷贝，和原件脱钩）
}

// Snapshot 返回任务当前状态的值拷贝视图；持锁调用。
func (t *Task) Snapshot() TaskView {
	files := make([]FileItem, len(t.Files)) // 新建值切片，长度等于原文件数
	for i, f := range t.Files {             // 遍历指针切片，f 是 *FileItem
		files[i] = *f                        // 解引用拷贝值，让视图和原件脱钩
	}
	return TaskView{
		ID:          t.ID,
		CreatedAt:   t.CreatedAt,
		Concurrency: t.Concurrency,
		Quality:     t.Quality,
		DirName:     t.DirName,
		Status:      t.Status,
		Done:        t.done.Load(),
		Elapsed:     t.Elapsed,
		Files:       files,
	}
}

// Store 是内存任务表。Go 的 map 并发读写会直接 fatal（进程崩溃，无法 recover），
// 所以对 tasks 的每次访问都必须持 mu——第一把锁，只护 map 结构。
type Store struct {
	mu    sync.Mutex
	tasks map[string]*Task
}

func New() *Store {
	// make 创建空 map；map 零值 nil 可以读但写会 panic，所以必须 make
	return &Store{tasks: make(map[string]*Task)}
}

func (s *Store) CreateTask(t *Task) {
	s.mu.Lock()         // 加锁保护 map（并发写 map 会 fatal）
	defer s.mu.Unlock() // 函数返回前释放锁
	s.tasks[t.ID] = t   // 把任务存入 map，键是任务 ID
}

// GetTask 只读入口：两段式加锁——先短暂持 s.mu 查指针就放，再持 t.mu 拍快照。
// 分两段是为了临界区最短：慢快照不会把其他任务的创建/查询堵在 s.mu 后面。
// 锁嵌套顺序全项目固定为 s.mu → t.mu（唯一嵌套点在 ListHistory），方向一致故无死锁。
func (s *Store) GetTask(id string) (TaskView, bool) {
	s.mu.Lock()            // 第一段：持 s.mu 查 map
	t, ok := s.tasks[id]   // comma-ok：ok 表示键是否存在
	s.mu.Unlock()          // 查完指针立即放锁，不让慢快照堵全局
	if !ok {
		return TaskView{}, false // 任务不存在
	}
	t.mu.Lock()              // 第二段：持任务自己的锁拍快照
	defer t.mu.Unlock()
	return t.Snapshot(), true // 返回值拷贝视图
}

// GetRef 供 service 在任务生命周期内持有可变实体并派发 worker；
// 只读方一律走 GetTask 快照，不得使用本方法。
func (s *Store) GetRef(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id] // 单值读：不存在返回 nil（*Task 的零值）
}

// ListHistory 遍历 map 生成摘要。注意循环体内还要拿每条任务的 t.mu：
// s.mu 只保证 map 结构不变，不保证单个 Task 的字段不被并发写。
// 此处是全项目唯一的锁嵌套点（s.mu → t.mu），顺序固定，无死锁风险。
func (s *Store) ListHistory() []TaskSummary {
	s.mu.Lock()            // 持 s.mu 遍历 map（遍历期间不能增删）
	defer s.mu.Unlock()
	out := make([]TaskSummary, 0, len(s.tasks)) // 预分配容量，避免 append 扩容
	for _, t := range s.tasks {                  // map 遍历顺序是随机的
		t.mu.Lock()                              // 循环内还要拿 t.mu：s.mu 只护 map 结构
		out = append(out, TaskSummary{
			ID:          t.ID,
			CreatedAt:   t.CreatedAt,
			Concurrency: t.Concurrency,
			Quality:     t.Quality,
			Status:      t.Status,
			Files:       len(t.Files),
			Done:        int(t.done.Load()),
			Elapsed:     t.Elapsed,
		})
		t.mu.Unlock() // 循环内手动 Unlock（不能用 defer，defer 要等函数返回）
	}
	return out
}
