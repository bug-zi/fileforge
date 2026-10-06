# fileforge designs-specs.md

> 本文档由 AI 基于 `docs/project/fileforge/design.md`（v0.1 草案：本地批量图片处理工具，Go 学习验收项目）生成，
> 是开发的直接依据。设计全记录见同目录 `2026-10-06-fileforge-design.md`。
> 依赖：运行时零外部服务；第三方依赖仅 Gin 与 `golang.org/x/image/draw`，其余全用标准库。

## 0. 命名与常量

| 项 | 值 |
|---|---|
| go module 名 | `fileforge` |
| 监听地址 | `:8080` |
| 缩放阈值 | `maxEdge = 1920`（最长边 > 1920 才缩放） |
| JPEG 质量 | `jpegQuality = 80` |
| 缩放算法 | `draw.CatmullRom` |
| 重命名格式 | `photo_%03d.jpg`（同任务内从 001 递增） |
| 文件存放 | `uploads/<taskID>/`（原始）、`outputs/<taskID>/`（结果），相对运行目录 |
| 任务 ID | `crypto/rand` 生成 16 位 hex |

文件状态枚举（`FileItem.Status`）：`pending` → `processing` → `done` / `duplicate` / `failed`。

API 一览：

| 方法与路径 | 作用 |
|---|---|
| `GET /` | 返回嵌入的单页前端（go:embed） |
| `POST /api/tasks` | multipart 上传（字段 `files[]` + `concurrency`），创建任务 |
| `GET /api/tasks/{id}` | 任务状态 JSON（SSE 降级备份） |
| `GET /api/tasks/{id}/events` | SSE 进度推送 |
| `GET /api/tasks/{id}/download` | zip 流式下载 done 文件 |
| `GET /api/history` | 历史任务耗时对比 |

## 1. 数据模型 / 状态（internal/store）

```go
type FileItem struct {
    OrigName string // 原始文件名
    Size     int64  // 原始字节数
    Path     string // uploads/ 下的存盘路径
    Status   string // pending|processing|done|duplicate|failed
    MD5      string // hex
    OutName  string // done 时：photo_001.jpg
    OutSize  int64  // done 时：结果字节数
    Reason   string // failed 时：失败原因
}

type Task struct {
    ID          string
    CreatedAt   time.Time
    Concurrency int
    Status      string // processing|done
    Files       []*FileItem
    done        atomic.Int64  // 完成计数（对外暴露 Done() int64）
    Elapsed     time.Duration // Status=done 时回填
}

// Store：sync.Mutex + map[string]*Task；另持 history 视图所需字段
// 方法：CreateTask(*Task)、GetTask(id) (*Task, bool)、ListHistory() []TaskSummary
type TaskSummary struct { ID string; CreatedAt time.Time; Concurrency int; Files, Done int; Elapsed time.Duration }
```

- 任务存内存，重启即清（有意设计，不做持久化）。
- `Task.Files` 元素在处理期只由所属 worker 写单个 `FileItem` 字段，读取方经 `GetTask` 快照式拷贝返回（拷贝时持锁），避免读半写状态。

## 2. 核心流程

### 2.1 上传建任务（handler → service，涉及 `internal/handler/task.go`、`internal/service/service.go`）

1. `POST /api/tasks`：`concurrency` 解析（钳位 1~32）；逐文件大小限制单文件 ≤ 50MB（防御性，超出记 failed 入表不落盘）；
2. 生成 taskID，建 `Task{Status: processing, Files: [...]}`，每个文件先落盘 `uploads/<taskID>/`（用 `io.Copy` 流式写，不用 Multipart 内存全量）；
3. `store.CreateTask` → 返回 `{"id": ..., "files": [...]}`；
4. `service.Run(task)`：异步启动 worker 池（goroutine 内执行，handler 立即返回）。

### 2.2 worker 池（service，答辩核心）

```go
jobs := make(chan *FileItem, len(task.Files))       // 带缓冲任务队列
var wg sync.WaitGroup
for i := 0; i < task.Concurrency; i++ {             // N = 滑块值
    wg.Add(1)
    go worker(jobs, task, &wg)                      // for it := range jobs { ... }
}
// 塞满 jobs 后 close(jobs)；wg.Wait() 后：task.Status=done、回填 Elapsed
```

worker 内单文件处理（processor 为纯函数，`internal/processor/processor.go`）：

1. `MD5` 全文件计算 → 与本任务已完成文件的 MD5 集比对（集合由 service 持 mutex 维护）：命中 → `duplicate`（仍归档原文件到 outputs，但**不进 zip**）；
   —— 修正：duplicate 不产生输出文件、不占 photo 序号，仅记录 MD5 与原文件引用；
2. 未命中 → 压缩流水线：`image.Decode` → 最长边 >1920 则 `draw.CatmullRom` 等比缩放 → `jpeg.Encode`（质量 80）→ 写 `outputs/<taskID>/photo_%03d.jpg`；
3. 解码失败（非图片/损坏）→ `failed` + 原因，仅保留 MD5，不进 zip；
4. 每文件收尾：`task.done.Add(1)` → SSE 广播进度。

PNG 透明通道：解码后若为 `*image.NRGBA`，直接 `jpeg.Encode`（Go 标准库对透明区按黑底处理，可接受——截图文字边缘轻微毛边已在 design 中取舍）。

### 2.3 SSE 进度（`internal/handler/sse.go`）

- `GET /api/tasks/{id}/events`：`Content-Type: text/event-stream`；service 持每任务一个 broadcaster（mutex + `[]chan []byte`）；
- 事件载荷：`{"done": n, "total": m, "last": {"name":..., "status":...}}`；任务 done 时推 `event: complete` 后关闭；
- 客户端断开（`r.Context().Done()`）即注销通道。

### 2.4 zip 下载（`internal/handler/download.go`）

- `GET /api/tasks/{id}/download`：遍历 `outputs/<taskID>/` 中 done 文件，`archive/zip` 流式写入 `w`（`w.Header set Content-Disposition attachment`），不落临时 zip。

### 2.5 历史表（store.ListHistory）

- 返回全部任务（含进行中）的 `TaskSummary`，前端按时间倒序渲染对比表。

### 2.6 优雅停机（`main.go`）

- `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` → `server.Shutdown(ctx)`：停止接新连接，等待在途请求完成；worker 池随进程退出（任务本就不持久化）。

## 3. 前端（`web/index.html` + `web/app.js`，原生 JS 单页，无框架无构建）

五块布局：拖拽区 / 并发数滑块（1~32）/ 文件状态列表 / 总进度条 + 下载按钮 / 历史耗时对比表。

- 拖拽区：dragover/drop 事件收集 `DataTransfer.files`；同时内置隐藏 `<input type="file" multiple>`（点击拖拽区触发，兜底 + 可测试性）；
- 上传：`fetch` + `FormData`（`files[]`、`concurrency`）；响应后渲染文件列表；
- 进度：优先 `EventSource` 订阅 SSE；失败自动降级为 1s 轮询 `GET /api/tasks/{id}`；
- 完成：显示下载按钮 + 三种结局统计（完成/重复/失败）；
- **图标一律不用 emoji**，用 https://fonts.google.com/icons 的 Material Symbols；**配色不使用彩亮饱和色**，围绕主题色低饱和色系。

## 4. 边界与错误处理

- 单文件失败不中断整批：worker 内 `defer recover()` + err 捕获，仅置该文件 `failed`；
- 重复文件：保留首个为 done，后续 `duplicate` 不进 zip、不占 photo 序号；
- 未知任务 ID：所有 `/api/tasks/{id}` 接口返回 404；
- 非法并发数：钳位到 [1, 32]；
- SSE 不支持/断开：前端降级轮询（进度功能永可退化，风险兜底）；
- 已知限制：EXIF 方向未处理（竖拍可能旋转错误），仅用户明确要求时才加。

## 5. 测试要求（落地判定：`go vet` + `go test` 全绿 + `go build` 产出可运行 exe）

- processor：真实测试图压缩后体积变小且最长边 ≤1920；同内容两文件 MD5 判重命中；损坏文件返回错误；
- store：并发读写无竞态（`go test -race ./...` 必须通过）；
- handler：上传→存盘→状态查询闭环用 `httptest` 覆盖。

---
## 增量注记（历轮修订在此追加，不删旧节）

> **261006 第1轮**：依据 design.md v0.1 草案与设计全记录首轮生成。
> **261006 实施修订（阶段 1）**：① §1 落地细化——`GetTask` 返回快照类型 `TaskView`（值拷贝），`Task` 暴露 `Lock/Unlock` 供 worker 更新 FileItem 时持同一把锁，`Done` 计数走 `atomic.Int64`；② §2.1 落地细化——同名上传文件自动加 `_1`/`_2` 序号后缀防互相覆盖（`OrigName` 存落盘名）；handler 层只依赖 service（`handler.New(svc)`），不直连 store；③ 前端坑位记录——gin `FileFromFS` 对嵌入 FS 会 301 循环，`GET /` 改用读出嵌入内容后 `c.Data` 返回；favicon 用内联 SVG data URL（现场演示不联网，图标字体 CDN 不可依赖，后续需要图标一律内联 SVG，见 §3 图标规则补充）。
> **261007 实施修订（阶段 2/3）**：① `Service.New` 签名为 `(store, uploadsDir, outputsDir)`，上传与输出目录分离；② store 新增 `GetRef(id)`——service 持可变任务实体专用，只读方仍走 `GetTask` 快照；③ processor 新增独立 `MD5Hex`：worker 先判重、命中即跳过压缩（重复文件不白压），`Process` 内部复用同一摘要；④ 交互语义定版：**上传即开跑**（POST /api/tasks 后 service 异步 RunByID，无独立「开始处理」键），与设计全记录 §4.3 数据流一致；⑤ 输出序号为完成序（atomic 递增），重复/失败文件不占号；⑥ Windows 下 `t.TempDir` 清理会与在途 worker 竞争——handler 测试统一以 `waitForTask` 等任务 done 再返回。
> **261007 实施修订（阶段 4 + design v0.2）**：① `Process(data, quality)` 质量入参（processor 兜底钳 1~100，handler 钳 50~95 缺省 80）；② store 的 Task/TaskView/TaskSummary 增加 `Quality`；③ SSE：service 持 `subs`（taskID → 订阅者集合），每文件终结推 `{done,total,last}`，Run 收尾推 `final:true` 帧；handler `/api/tasks/:id/events` 订阅时已 done 则立即补发 final（覆盖小任务快过建连的窗口），断开即注销；④ 前端 EventSource 优先，`onerror` 降级 1s 轮询，final 后拉一次全量文件列表修正各行为终态。
