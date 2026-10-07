package service

import (
	"crypto/rand"     // 加密安全随机数，用于生成任务 ID
	"encoding/hex"    // 字节转十六进制字符串
	"encoding/json"   // JSON 序列化（SSE 帧、final 帧）
	"fmt"             // 格式化输出、错误包装
	"io"              // io.Copy 流式拷贝（上传落盘）
	"mime/multipart"  // 处理 multipart 上传文件头
	"os"              // 文件操作（读/写/建目录）
	"path/filepath"   // 跨平台路径拼接
	"runtime"         // runtime.NumCPU 获取逻辑核心数
	"strings"         // 字符串处理（sanitizeName）
	"sync"            // Mutex、WaitGroup
	"sync/atomic"     // 原子计数器（seq、total）
	"time"            // 时间、耗时

	"fileforge/internal/processor" // 图像处理纯函数
	"fileforge/internal/store"     // 内存任务表
)

// Service 负责任务生命周期：建任务（存盘 + 入表）、分发 job、汇总结果、广播进度。
type Service struct {
	store   *store.Store // 内存任务表（依赖注入）
	uploads string       // 原始文件根目录 uploads/
	outputs string       // 处理结果根目录 outputs/

	subMu sync.Mutex                                   // 保护 subs map
	subs  map[string]map[chan []byte]struct{} // taskID -> SSE 订阅者集合（空结构体当集合用，零内存）
}

func New(st *store.Store, uploadsDir, outputsDir string) *Service {
	// 依赖注入：store 由 main 创建后传入，service 不自己 new store
	return &Service{store: st, uploads: uploadsDir, outputs: outputsDir}
}

// CreateTask 将上传文件流式落盘到 uploads/<taskID>/，以「待确认」状态入表。
// 并发数与质量由开发者在开始转换时提交（Reserve），预览大小走 Estimate。
func (s *Service) CreateTask(fhs []*multipart.FileHeader) (store.TaskView, error) {
	id, err := newTaskID() // 生成随机任务 ID（16 位 hex）
	if err != nil {
		return store.TaskView{}, fmt.Errorf("生成任务 ID: %w", err)
	}
	dir := filepath.Join(s.uploads, id) // uploads/{taskID}/
	if err := os.MkdirAll(dir, 0o755); err != nil { // 0o755 = 所有者可读写执行，其他可读执行
		return store.TaskView{}, fmt.Errorf("创建任务目录: %w", err)
	}
	// 预留时间命名的输出目录（同时防同分钟冲突）
	dirName, err := s.reserveOutDir(time.Now())
	if err != nil {
		return store.TaskView{}, fmt.Errorf("创建输出目录: %w", err)
	}

	task := &store.Task{ // 创建任务实体，状态为 ready（待确认）
		ID:        id,
		CreatedAt: time.Now(),
		DirName:   dirName,
		Status:    store.TaskReady,
	}
	for _, fh := range fhs { // 逐个保存上传文件
		item, err := s.saveFile(dir, fh) // 流式落盘，返回 FileItem
		if err != nil {
			return store.TaskView{}, fmt.Errorf("保存文件 %q: %w", fh.Filename, err)
		}
		task.Files = append(task.Files, item) // 追加到任务的文件列表
	}

	s.store.CreateTask(task)     // 存入 store
	view, _ := s.store.GetTask(id) // 取出快照返回给 handler
	return view, nil
}

func (s *Service) Task(id string) (store.TaskView, bool) {
	return s.store.GetTask(id) // 透传：查任务快照
}

func (s *Service) ListHistory() []store.TaskSummary {
	return s.store.ListHistory() // 透传：历史列表
}

// OutFile 是一个待打包的处理结果。
type OutFile struct {
	Name string // 文件名（photo_001.jpg）
	Path string // 完整磁盘路径
}

// DoneFiles 列出任务的全部结果文件（只含处理成功写入 outputs 的文件）。
func (s *Service) DoneFiles(dirName string) []OutFile {
	entries, err := os.ReadDir(filepath.Join(s.outputs, dirName)) // 读输出目录
	if err != nil {
		return nil // 目录不存在或读失败，返回 nil
	}
	out := make([]OutFile, 0, len(entries)) // 预分配容量
	for _, e := range entries {
		if e.IsDir() {
			continue // 跳过子目录
		}
		out = append(out, OutFile{Name: e.Name(), Path: filepath.Join(s.outputs, dirName, e.Name())})
	}
	return out
}

// FileEstimate 是单文件在指定质量下的精确输出大小（真实编码一次）。
type FileEstimate struct {
	Name string // 原始文件名
	Size int64  // 原始文件大小
	Est  int64  // 压缩后的预估大小（真实编码一次得到的精确值）
	Err  string // 错误信息（空表示成功）
}

// Estimate 按质量对每个待确认文件真实编码一次，返回精确输出大小（不写盘）。
// 仅待确认任务可用；有界 worker 池并发编码。
func (s *Service) Estimate(id string, quality int) ([]FileEstimate, int64, error) {
	task := s.store.GetRef(id) // 拿可变实体（写通道）
	if task == nil {
		return nil, 0, fmt.Errorf("任务 %q 不存在", id)
	}
	task.Lock()
	if task.Status != store.TaskReady { // 只允许待确认任务预览
		task.Unlock()
		return nil, 0, fmt.Errorf("任务已开始或已完成")
	}
	items := make([]*store.FileItem, len(task.Files))
	copy(items, task.Files) // 拷贝指针切片，避免持锁期间做耗时操作
	task.Unlock()

	results := make([]FileEstimate, len(items)) // 按索引存结果，保证顺序
	var total atomic.Int64                       // 总输出大小（原子累加）
	var wg sync.WaitGroup                        // 等待所有 worker 完成
	workers := runtime.NumCPU()                  // 逻辑 CPU 核心数
	if workers > 8 {
		workers = 8 // 上限 8：预览是 CPU 密集，开太多反而因上下文切换变慢
	}
	if workers < 1 {
		workers = 1
	}
	jobs := make(chan int) // 无缓冲 channel：worker 取索引
	for range workers {    // 启动 workers 个 worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs { // 消费索引
				item := items[i]
				data, err := os.ReadFile(item.Path) // 读文件
				if err != nil {
					results[i] = FileEstimate{Name: item.OrigName, Size: item.Size, Err: "读取文件失败: " + err.Error()}
					continue
				}
				res, err := processor.Process(data, quality) // 真实编码一次（不写盘）
				if err != nil {
					results[i] = FileEstimate{Name: item.OrigName, Size: item.Size, Err: err.Error()}
					continue
				}
				results[i] = FileEstimate{Name: item.OrigName, Size: item.Size, Est: int64(len(res.OutBytes))}
				total.Add(int64(len(res.OutBytes))) // 原子累加到总大小
			}
		}()
	}
	for i := range items { // 生产者：把所有索引发给 worker
		jobs <- i
	}
	close(jobs) // 关闭 channel，worker 的 for range 自动退出
	wg.Wait()   // 等所有 worker 完成
	return results, total.Load(), nil
}

// reserveOutDir 以 YYMMDD-HHMM 命名预留输出目录；同分钟冲突追加 -2、-3。
func (s *Service) reserveOutDir(t time.Time) (string, error) {
	base := t.Format("060102-1504") // Go 时间格式必须用 2006-01-02 15:04:05 这个参考时间的数字
	name := base
	for i := 2; ; i++ { // 同分钟冲突时追加 -2、-3...
		dir := filepath.Join(s.outputs, name)
		if _, err := os.Stat(dir); os.IsNotExist(err) { // 目录不存在
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", err
			}
			return name, nil // 创建成功，返回目录名
		}
		name = fmt.Sprintf("%s-%d", base, i) // 已存在，试下一个序号
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
	if task.Status != store.TaskReady { // 只允许 ready 状态开始
		return store.TaskView{}, fmt.Errorf("任务已开始或已完成，不能重复开始")
	}
	task.Concurrency = concurrency          // 写入用户指定的并发数
	task.Quality = quality                   // 写入 JPEG 质量
	task.Status = store.TaskProcessing       // 状态改为 processing
	return task.Snapshot(), nil              // 返回快照
}

// Start 是便捷入口：预留（校验 + 写参数）后同步执行到完成。
func (s *Service) Start(id string, concurrency, quality int) (store.TaskView, error) {
	view, err := s.Reserve(id, concurrency, quality) // 校验 + 写参数
	if err != nil {
		return view, err
	}
	if err := s.RunByID(id); err != nil { // 同步执行到完成
		return view, err
	}
	return view, nil
}

// Subscribe 注册任务进度订阅者；cancel 必须在连接结束时调用。
func (s *Service) Subscribe(id string) (<-chan []byte, func(), bool) {
	if _, ok := s.store.GetTask(id); !ok { // 任务不存在则拒绝订阅
		return nil, nil, false
	}
	ch := make(chan []byte, 16) // 带缓冲 16 的 channel：慢订阅者有一定缓冲空间
	s.subMu.Lock()
	if s.subs == nil { // 首次订阅，初始化外层 map
		s.subs = make(map[string]map[chan []byte]struct{})
	}
	m := s.subs[id]
	if m == nil { // 该任务还没有订阅者，新建内层 map
		m = make(map[chan []byte]struct{})
		s.subs[id] = m
	}
	m[ch] = struct{}{} // 注册订阅者（空结构体当集合 value）
	s.subMu.Unlock()

	cancel := func() { // 连接断开时调用，从订阅表删除
		s.subMu.Lock()
		if m := s.subs[id]; m != nil {
			delete(m, ch)
		}
		s.subMu.Unlock()
	}
	return ch, cancel, true // 返回只读 channel + 取消函数 + 成功标记
}

func (s *Service) notify(taskID string, payload []byte) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs[taskID] { // 遍历该任务的所有订阅者
		select {
		case ch <- payload: // 尝试发送
		default:            // channel 满了，丢帧（不阻塞，避免慢客户端拖垮广播）
		}
	}
}

func (s *Service) closeSubs(taskID string) {
	s.subMu.Lock()
	delete(s.subs, taskID) // 删除整个任务的订阅表（不 close 各 channel，避免并发 panic）
	s.subMu.Unlock()
}

// RunByID 同步执行任务（handler 以 goroutine 调用）。
func (s *Service) RunByID(id string) error {
	task := s.store.GetRef(id) // 拿可变实体
	if task == nil {
		return fmt.Errorf("任务 %q 不存在", id)
	}
	s.Run(task) // 执行 worker 池
	return nil
}

// Run 是并发模型核心（答辩重点）：
// 带缓冲 channel 做任务队列，worker 池（大小 = 任务并发数）消费，
// WaitGroup 收尾，atomic 序号分配输出文件名，单文件 recover 隔离，
// 每个文件终结即向 SSE 订阅者广播进度，全部完成后广播 final 帧。
func (s *Service) Run(task *store.Task) {
	start := time.Now() // 记录开始时间，Finish 时算总耗时

	var (
		seenMu sync.Mutex                       // 保护 seen map 的锁
		seen   = make(map[string]bool)          // 已出现的 MD5，先到先得（查+写复合操作，必须用锁）
		seq    atomic.Int64                     // 输出文件序号（完成序，单变量用 atomic）
		wg     sync.WaitGroup                   // 等待所有 worker 退出
	)

	jobs := make(chan *store.FileItem, len(task.Files)) // 带缓冲 channel 当任务队列，缓冲=文件数
	for range task.Concurrency {                       // 启动 N 个 worker（N=用户并发数）
		wg.Add(1)                                       // Add 必须在 goroutine 外调用
		go func() {
			defer wg.Done()                              // worker 退出时计数 -1
			for item := range jobs {                     // 消费到 channel 关闭
				s.processOne(task, item, &seenMu, seen, &seq) // 处理单个文件
			}
		}()
	}
	for _, item := range task.Files { // 生产者：把所有文件塞进 channel
		jobs <- item
	}
	close(jobs) // 关闭 channel：广播"没活了"，worker 的 for range 自动退出
	wg.Wait()   // 等所有 worker 退出（所有文件处理完毕）

	task.Finish(time.Since(start)) // 收尾：状态置 done + 记录耗时
	final, _ := json.Marshal(map[string]any{
		"done": len(task.Files), "total": len(task.Files), "final": true, // final 帧：进度 100%
	})
	s.notify(task.ID, final)   // 广播 final 帧
	s.closeSubs(task.ID)        // 关闭该任务的所有订阅
}

func (s *Service) processOne(
	task *store.Task,
	item *store.FileItem,
	seenMu *sync.Mutex,
	seen map[string]bool,
	seq *atomic.Int64,
) {
	// recover 单文件隔离：一个文件 panic 只废这一个，不影响整批
	defer func() {
		if r := recover(); r != nil {
			s.failItem(task, item, "", fmt.Sprintf("内部错误: %v", r))
		}
	}()

	data, err := os.ReadFile(item.Path) // 读取原始文件
	if err != nil {
		s.failItem(task, item, "", "读取文件失败: "+err.Error())
		return
	}

	// 先算 MD5 判重：命中的重复文件直接跳过压缩（省 CPU）
	sum := processor.MD5Hex(data)
	seenMu.Lock()            // 持锁：查+写是复合操作
	dup := seen[sum]         // 查这个 MD5 是否出现过
	if !dup {
		seen[sum] = true     // 没出现过，标记为已出现
	}
	seenMu.Unlock()
	if dup { // 重复文件
		task.Lock()
		item.MD5 = sum
		item.Status = store.StatusDuplicate
		task.Unlock()
		s.finishItem(task, item.OrigName, store.StatusDuplicate, "") // 计数+广播
		return
	}

	res, err := processor.Process(data, task.Quality) // 解码→缩放→JPEG 重编码
	if err != nil {
		s.failItem(task, item, sum, err.Error())
		return
	}

	name := processor.OutName(int(seq.Add(1)))   // 原子自增序号，生成 photo_NNN.jpg
	outDir := filepath.Join(s.outputs, task.DirName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		s.failItem(task, item, sum, "创建输出目录失败: "+err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(outDir, name), res.OutBytes, 0o644); err != nil { // 0o644 = 所有者可读写，其他只读
		s.failItem(task, item, sum, "写出结果失败: "+err.Error())
		return
	}

	task.Lock() // 持 task 锁更新 FileItem 多字段（和读方 GetTask 互斥）
	item.MD5 = sum
	item.Status = store.StatusDone
	item.OutName = name
	item.OutSize = int64(len(res.OutBytes))
	task.Unlock()
	s.finishItem(task, item.OrigName, store.StatusDone, "") // 计数+广播进度帧
}

// finishItem 完成一个文件的计数并广播进度帧。
func (s *Service) finishItem(task *store.Task, name, status, reason string) {
	task.AddDone() // 完成计数 +1（atomic）
	frame := map[string]any{ // SSE 进度帧
		"done":  task.Done(),     // 已完成数
		"total": len(task.Files), // 总数
		"last": map[string]string{ // 最后完成的文件信息
			"name":   name,
			"status": status,
			"reason": reason,
		},
	}
	b, _ := json.Marshal(frame) // 序列化成 JSON 字节
	s.notify(task.ID, b)         // 广播给所有 SSE 订阅者
}

// failItem 终结一个文件为 failed 状态（md5 可为空，尚未算出时）。
func (s *Service) failItem(task *store.Task, item *store.FileItem, md5sum, reason string) {
	task.Lock() // 持锁更新失败状态
	if md5sum != "" { // 有的失败发生在算 MD5 之前，md5sum 为空
		item.MD5 = md5sum
	}
	item.Status = store.StatusFailed
	item.Reason = reason // 失败原因（前端展示）
	task.Unlock()
	s.finishItem(task, item.OrigName, store.StatusFailed, reason) // 计数+广播
}

// saveFile 落盘单个上传文件；同名冲突自动加序号后缀，绝不互相覆盖。
func (s *Service) saveFile(dir string, fh *multipart.FileHeader) (*store.FileItem, error) {
	orig := sanitizeName(fh.Filename) // 剥路径成分，防目录穿越
	name := orig
	dst := filepath.Join(dir, name)
	for i := 1; ; i++ { // 同名冲突：a.jpg → a_1.jpg → a_2.jpg...
		if _, err := os.Stat(dst); os.IsNotExist(err) { // 文件不存在，可用
			break
		}
		ext := filepath.Ext(orig)                                        // 取扩展名
		name = fmt.Sprintf("%s_%d%s", strings.TrimSuffix(orig, ext), i, ext) // 加序号
		dst = filepath.Join(dir, name)
	}

	src, err := fh.Open() // 打开上传文件流
	if err != nil {
		return nil, err
	}
	defer src.Close()

	out, err := os.Create(dst) // 创建目标文件
	if err != nil {
		return nil, err
	}
	defer out.Close()

	size, err := io.Copy(out, src) // 流式拷贝：边读边写，不占中间内存
	if err != nil {
		return nil, err
	}
	return &store.FileItem{OrigName: name, Size: size, Path: dst, Status: store.StatusPending}, nil
}

// sanitizeName 剥离上传文件名中的任何路径成分，防目录穿越。
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/") // 统一路径分隔符为 /（兼容 Windows）
	if i := strings.LastIndex(name, "/"); i >= 0 { // 取最后一个 / 之后的部分
		name = name[i+1:] // 剥掉所有路径成分，只留文件名
	}
	if name == "" || name == "." { // 空名或只有点，给个默认名
		name = "file"
	}
	return name
}

func newTaskID() (string, error) {
	b := make([]byte, 8) // 8 字节随机数
	if _, err := rand.Read(b); err != nil { // 加密安全随机
		return "", err
	}
	return hex.EncodeToString(b), nil // 转 16 位 hex 字符串
}
