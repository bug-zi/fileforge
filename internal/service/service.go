package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fileforge/internal/processor"
	"fileforge/internal/store"
)

// Service 负责任务生命周期：建任务（存盘 + 入表）、分发 job、汇总结果、广播进度。
type Service struct {
	store   *store.Store
	uploads string // 原始文件根目录
	outputs string // 处理结果根目录

	subMu sync.Mutex
	subs  map[string]map[chan []byte]struct{} // taskID -> SSE 订阅者
}

func New(st *store.Store, uploadsDir, outputsDir string) *Service {
	return &Service{store: st, uploads: uploadsDir, outputs: outputsDir}
}

// CreateTask 将上传文件流式落盘到 uploads/<taskID>/，以「待确认」状态入表。
// 并发数与质量由开发者在开始转换时提交（Reserve），预览大小走 Estimate。
func (s *Service) CreateTask(fhs []*multipart.FileHeader) (store.TaskView, error) {
	id, err := newTaskID()
	if err != nil {
		return store.TaskView{}, fmt.Errorf("生成任务 ID: %w", err)
	}
	dir := filepath.Join(s.uploads, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.TaskView{}, fmt.Errorf("创建任务目录: %w", err)
	}
	// 预留时间命名的输出目录（同时防同分钟冲突）
	dirName, err := s.reserveOutDir(time.Now())
	if err != nil {
		return store.TaskView{}, fmt.Errorf("创建输出目录: %w", err)
	}

	task := &store.Task{
		ID:        id,
		CreatedAt: time.Now(),
		DirName:   dirName,
		Status:    store.TaskReady,
	}
	for _, fh := range fhs {
		item, err := s.saveFile(dir, fh)
		if err != nil {
			return store.TaskView{}, fmt.Errorf("保存文件 %q: %w", fh.Filename, err)
		}
		task.Files = append(task.Files, item)
	}

	s.store.CreateTask(task)
	view, _ := s.store.GetTask(id)
	return view, nil
}

func (s *Service) Task(id string) (store.TaskView, bool) {
	return s.store.GetTask(id)
}

func (s *Service) ListHistory() []store.TaskSummary {
	return s.store.ListHistory()
}

// OutFile 是一个待打包的处理结果。
type OutFile struct {
	Name string
	Path string
}

// DoneFiles 列出任务的全部结果文件（只含处理成功写入 outputs 的文件）。
func (s *Service) DoneFiles(dirName string) []OutFile {
	entries, err := os.ReadDir(filepath.Join(s.outputs, dirName))
	if err != nil {
		return nil
	}
	out := make([]OutFile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out = append(out, OutFile{Name: e.Name(), Path: filepath.Join(s.outputs, dirName, e.Name())})
	}
	return out
}

// FileEstimate 是单文件在指定质量下的精确输出大小（真实编码一次）。
type FileEstimate struct {
	Name string
	Size int64
	Est  int64
	Err  string
}

// Estimate 按质量对每个待确认文件真实编码一次，返回精确输出大小（不写盘）。
// 仅待确认任务可用；有界 worker 池并发编码。
func (s *Service) Estimate(id string, quality int) ([]FileEstimate, int64, error) {
	task := s.store.GetRef(id)
	if task == nil {
		return nil, 0, fmt.Errorf("任务 %q 不存在", id)
	}
	task.Lock()
	if task.Status != store.TaskReady {
		task.Unlock()
		return nil, 0, fmt.Errorf("任务已开始或已完成")
	}
	items := make([]*store.FileItem, len(task.Files))
	copy(items, task.Files)
	task.Unlock()

	results := make([]FileEstimate, len(items))
	var total atomic.Int64
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				item := items[i]
				data, err := os.ReadFile(item.Path)
				if err != nil {
					results[i] = FileEstimate{Name: item.OrigName, Size: item.Size, Err: "读取文件失败: " + err.Error()}
					continue
				}
				res, err := processor.Process(data, quality)
				if err != nil {
					results[i] = FileEstimate{Name: item.OrigName, Size: item.Size, Err: err.Error()}
					continue
				}
				results[i] = FileEstimate{Name: item.OrigName, Size: item.Size, Est: int64(len(res.OutBytes))}
				total.Add(int64(len(res.OutBytes)))
			}
		}()
	}
	for i := range items {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results, total.Load(), nil
}

// reserveOutDir 以 YYMMDD-HHMM 命名预留输出目录；同分钟冲突追加 -2、-3。
func (s *Service) reserveOutDir(t time.Time) (string, error) {
	base := t.Format("060102-1504")
	name := base
	for i := 2; ; i++ {
		dir := filepath.Join(s.outputs, name)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			return name, nil
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

// Reserve 将待确认任务标记为开始，写入并发数与质量；非 ready 任务拒绝。
func (s *Service) Reserve(id string, concurrency, quality int) (store.TaskView, error) {
	task := s.store.GetRef(id)
	if task == nil {
		return store.TaskView{}, fmt.Errorf("任务 %q 不存在", id)
	}
	task.Lock()
	defer task.Unlock()
	if task.Status != store.TaskReady {
		return store.TaskView{}, fmt.Errorf("任务已开始或已完成，不能重复开始")
	}
	task.Concurrency = concurrency
	task.Quality = quality
	task.Status = store.TaskProcessing
	return task.Snapshot(), nil
}

// Start 是便捷入口：预留（校验 + 写参数）后同步执行到完成。
func (s *Service) Start(id string, concurrency, quality int) (store.TaskView, error) {
	view, err := s.Reserve(id, concurrency, quality)
	if err != nil {
		return view, err
	}
	if err := s.RunByID(id); err != nil {
		return view, err
	}
	return view, nil
}

// Subscribe 注册任务进度订阅者；cancel 必须在连接结束时调用。
func (s *Service) Subscribe(id string) (<-chan []byte, func(), bool) {
	if _, ok := s.store.GetTask(id); !ok {
		return nil, nil, false
	}
	ch := make(chan []byte, 16)
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = make(map[string]map[chan []byte]struct{})
	}
	m := s.subs[id]
	if m == nil {
		m = make(map[chan []byte]struct{})
		s.subs[id] = m
	}
	m[ch] = struct{}{}
	s.subMu.Unlock()

	cancel := func() {
		s.subMu.Lock()
		if m := s.subs[id]; m != nil {
			delete(m, ch)
		}
		s.subMu.Unlock()
	}
	return ch, cancel, true
}

func (s *Service) notify(taskID string, payload []byte) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs[taskID] {
		select {
		case ch <- payload:
		default: // 订阅方堵塞时丢帧，靠 final 帧兜底
		}
	}
}

func (s *Service) closeSubs(taskID string) {
	s.subMu.Lock()
	delete(s.subs, taskID)
	s.subMu.Unlock()
}

// RunByID 同步执行任务（handler 以 goroutine 调用）。
func (s *Service) RunByID(id string) error {
	task := s.store.GetRef(id)
	if task == nil {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	s.Run(task)
	return nil
}

// Run 是并发模型核心（答辩重点）：
// 带缓冲 channel 做任务队列，worker 池（大小 = 任务并发数）消费，
// WaitGroup 收尾，atomic 序号分配输出文件名，单文件 recover 隔离，
// 每个文件终结即向 SSE 订阅者广播进度，全部完成后广播 final 帧。
func (s *Service) Run(task *store.Task) {
	start := time.Now()

	var (
		seenMu sync.Mutex
		seen   = make(map[string]bool) // 已出现的 MD5，先到先得
		seq    atomic.Int64            // 输出文件序号（完成序）
		wg     sync.WaitGroup
	)

	jobs := make(chan *store.FileItem, len(task.Files))
	for range task.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				s.processOne(task, item, &seenMu, seen, &seq)
			}
		}()
	}
	for _, item := range task.Files {
		jobs <- item
	}
	close(jobs)
	wg.Wait()

	task.Finish(time.Since(start))
	final, _ := json.Marshal(map[string]any{
		"done": len(task.Files), "total": len(task.Files), "final": true,
	})
	s.notify(task.ID, final)
	s.closeSubs(task.ID)
}

func (s *Service) processOne(
	task *store.Task,
	item *store.FileItem,
	seenMu *sync.Mutex,
	seen map[string]bool,
	seq *atomic.Int64,
) {
	defer func() {
		if r := recover(); r != nil {
			s.failItem(task, item, "", fmt.Sprintf("内部错误: %v", r))
		}
	}()

	data, err := os.ReadFile(item.Path)
	if err != nil {
		s.failItem(task, item, "", "读取文件失败: "+err.Error())
		return
	}

	// 先算 MD5 判重：命中的重复文件直接跳过压缩
	sum := processor.MD5Hex(data)
	seenMu.Lock()
	dup := seen[sum]
	if !dup {
		seen[sum] = true
	}
	seenMu.Unlock()
	if dup {
		task.Lock()
		item.MD5 = sum
		item.Status = store.StatusDuplicate
		task.Unlock()
		s.finishItem(task, item.OrigName, store.StatusDuplicate, "")
		return
	}

	res, err := processor.Process(data, task.Quality)
	if err != nil {
		s.failItem(task, item, sum, err.Error())
		return
	}

	name := processor.OutName(int(seq.Add(1)))
	outDir := filepath.Join(s.outputs, task.DirName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		s.failItem(task, item, sum, "创建输出目录失败: "+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(outDir, name), res.OutBytes, 0o644); err != nil {
		s.failItem(task, item, sum, "写出结果失败: "+err.Error())
		return
	}

	task.Lock()
	item.MD5 = sum
	item.Status = store.StatusDone
	item.OutName = name
	item.OutSize = int64(len(res.OutBytes))
	task.Unlock()
	s.finishItem(task, item.OrigName, store.StatusDone, "")
}

// finishItem 完成一个文件的计数并广播进度帧。
func (s *Service) finishItem(task *store.Task, name, status, reason string) {
	task.AddDone()
	frame := map[string]any{
		"done":  task.Done(),
		"total": len(task.Files),
		"last": map[string]string{
			"name":   name,
			"status": status,
			"reason": reason,
		},
	}
	b, _ := json.Marshal(frame)
	s.notify(task.ID, b)
}

// failItem 终结一个文件为 failed 状态（md5 可为空，尚未算出时）。
func (s *Service) failItem(task *store.Task, item *store.FileItem, md5sum, reason string) {
	task.Lock()
	if md5sum != "" {
		item.MD5 = md5sum
	}
	item.Status = store.StatusFailed
	item.Reason = reason
	task.Unlock()
	s.finishItem(task, item.OrigName, store.StatusFailed, reason)
}

// saveFile 落盘单个上传文件；同名冲突自动加序号后缀，绝不互相覆盖。
func (s *Service) saveFile(dir string, fh *multipart.FileHeader) (*store.FileItem, error) {
	orig := sanitizeName(fh.Filename)
	name := orig
	dst := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		ext := filepath.Ext(orig)
		name = fmt.Sprintf("%s_%d%s", strings.TrimSuffix(orig, ext), i, ext)
		dst = filepath.Join(dir, name)
	}

	src, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close()

	out, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer out.Close()

	size, err := io.Copy(out, src)
	if err != nil {
		return nil, err
	}
	return &store.FileItem{OrigName: name, Size: size, Path: dst, Status: store.StatusPending}, nil
}

// sanitizeName 剥离上传文件名中的任何路径成分，防目录穿越。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" || name == "." {
		name = "file"
	}
	return name
}

func newTaskID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
