package handler

import (
	"archive/zip"   // zip 流式打包
	"encoding/json" // JSON 序列化（SSE final 帧兜底）
	"fmt"           // 格式化字符串
	"io"            // io.Copy 流式拷贝（zip 下载）
	"net/http"      // HTTP 状态码常量
	"os"            // os.Open 读文件（zip 下载）
	"strconv"       // 字符串转整数（钳位参数）
	"strings"       // 字符串包含判断（SSE final 帧检测）

	"github.com/gin-gonic/gin" // Web 框架

	"fileforge/internal/processor" // 取 DefaultQuality 常量
	"fileforge/internal/service"   // 业务层
	"fileforge/internal/store"     // 状态常量（TaskReady 等）
)

const (
	minConcurrency = 1   // 并发数下限
	maxConcurrency = 32  // 并发数上限
	minQuality     = 1   // JPEG 质量下限
	maxQuality     = 100 // JPEG 质量上限
)

type Handler struct {
	svc *service.Service // 依赖注入：业务层
}

func New(svc *service.Service) *gin.Engine {
	h := &Handler{svc: svc}      // 创建 handler 实例
	r := gin.New()                // 新建 Gin 引擎（不用 Default，手动加中间件）
	r.Use(gin.Recovery())         // panic 恢复中间件：单个请求 panic 不崩进程
	r.POST("/api/tasks", h.createTask)              // 上传建任务
	r.POST("/api/tasks/:id/start", h.startTask)    // 开始转换
	r.GET("/api/tasks/:id", h.getTask)              // 查任务详情
	r.GET("/api/tasks/:id/events", h.events)       // SSE 实时进度
	r.GET("/api/tasks/:id/estimate", h.estimate)   // 压缩预览
	r.GET("/api/tasks/:id/download", h.download)   // 下载 zip
	r.GET("/api/history", h.history)                // 历史列表
	return r
}

func (h *Handler) createTask(c *gin.Context) {
	form, err := c.MultipartForm() // 解析 multipart 表单
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 multipart 表单"}) // 400
		return
	}
	fhs := form.File["files[]"] // 取上传文件列表（前端字段名 files[]）
	if len(fhs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未收到任何文件"})
		return
	}

	// 只入库不处理：任务进入待确认，预览与开始转换走后续接口
	view, err := h.svc.CreateTask(fhs) // 落盘 + 入表
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()}) // 500
		return
	}
	c.JSON(http.StatusOK, toTaskJSON(view)) // 200，返回任务 JSON
}

// startTask 开始转换：写入并发数与质量后异步执行 worker 池。
func (h *Handler) startTask(c *gin.Context) {
	id := c.Param("id") // 取路径参数 :id
	if _, ok := h.svc.Task(id); !ok { // 校验任务存在
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"}) // 404
		return
	}
	// 钳位并发数和质量（不信任前端输入），然后 Reserve 写入参数
	view, err := h.svc.Reserve(id, clampConcurrency(c.PostForm("concurrency")), clampQuality(c.PostForm("quality")))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()}) // 409 状态冲突
		return
	}
	go func() { _ = h.svc.RunByID(id) }() // 异步启动 worker 池，不阻塞 HTTP 请求
	c.JSON(http.StatusOK, toTaskJSON(view)) // 立即返回 200，进度走 SSE
}

// estimate 返回待确认任务在指定质量下的精确输出大小预览。
func (h *Handler) estimate(c *gin.Context) {
	id := c.Param("id")
	view, ok := h.svc.Task(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	if view.Status != store.TaskReady { // 只允许待确认任务预览
		c.JSON(http.StatusConflict, gin.H{"error": "任务已开始或已完成"}) // 409
		return
	}
	quality := clampQuality(c.Query("quality")) // 取 URL 查询参数 ?quality=80
	files, total, err := h.svc.Estimate(id, quality) // 真实编码一次算大小
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	out := make([]estimateFileJSON, 0, len(files)) // 转成前端 JSON 结构
	for _, f := range files {
		out = append(out, estimateFileJSON{
			OrigName: f.Name,
			Size:     f.Size,
			Est:      f.Est,
			Err:      f.Err,
		})
	}
	c.JSON(http.StatusOK, gin.H{"quality": quality, "total": total, "files": out})
}

// events 是 SSE 进度推送：每个文件终结一帧，任务完成发 complete 帧后关闭。
// 客户端不支持/断开时，前端自动降级为轮询。
func (h *Handler) events(c *gin.Context) {
	id := c.Param("id")
	view, ok := h.svc.Task(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}

	c.Header("Content-Type", "text/event-stream") // SSE 响应头
	c.Header("Cache-Control", "no-cache")          // 不缓存进度帧

	ch, cancel, _ := h.svc.Subscribe(id) // 订阅进度
	if cancel != nil {
		defer cancel() // 连接结束时取消订阅
	}

	// writeFrame 写一帧 SSE 数据并 Flush
	writeFrame := func(payload []byte, final bool) {
		if final {
			c.Writer.WriteString("event: complete\ndata: ") // 命名事件 complete
		} else {
			c.Writer.WriteString("data: ") // 普通 data 帧
		}
		c.Writer.Write(payload)
		c.Writer.WriteString("\n\n") // 空行表示一帧结束
		c.Writer.Flush()              // 立即推送，不缓冲
	}

	// 订阅时任务已完成（小任务可能比建连更快）：立即补发 final
	if view.Status == store.TaskDone {
		final, _ := json.Marshal(map[string]any{"done": len(view.Files), "total": len(view.Files), "final": true})
		writeFrame(final, true)
		return
	}

	for { // 长连接循环
		select {
		case <-c.Request.Context().Done(): // 客户端断开（关页面/断网）
			return
		case payload, open := <-ch: // 收到 service 广播的帧
			if !open { // channel 关闭
				return
			}
			final := strings.Contains(string(payload), `"final":true`) // 检测是否 final 帧
			writeFrame(payload, final)
			if final { // final 帧后关闭连接
				return
			}
		}
	}
}

func (h *Handler) getTask(c *gin.Context) {
	view, ok := h.svc.Task(c.Param("id")) // 查任务快照
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"}) // 404
		return
	}
	c.JSON(http.StatusOK, toTaskJSON(view)) // 200，SSE 断了的兜底轮询接口
}

func (h *Handler) download(c *gin.Context) {
	id := c.Param("id")
	view, ok := h.svc.Task(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}

	c.Header("Content-Type", "application/zip")                                                                 // ZIP 响应头
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="fileforge-%s.zip"`, view.DirName)) // 下载文件名

	zw := zip.NewWriter(c.Writer) // 直接写到 HTTP 响应流（流式打包，不占中间内存）
	for _, f := range h.svc.DoneFiles(view.DirName) { // 遍历所有成功的输出文件
		w, err := zw.Create(f.Name) // 在 zip 里创建一个文件
		if err != nil {
			continue
		}
		src, err := os.Open(f.Path) // 打开输出文件
		if err != nil {
			continue
		}
		_, _ = io.Copy(w, src) // 边读边写进 zip 流
		src.Close()
	}
	zw.Close() // 写完 zip 目录结构，Flush 到响应
}

func (h *Handler) history(c *gin.Context) {
	sums := h.svc.ListHistory() // 历史摘要列表
	out := make([]historyJSON, 0, len(sums))
	for _, s := range sums {
		out = append(out, historyJSON{
			ID:          s.ID,
			CreatedAt:   s.CreatedAt.Format("2006-01-02 15:04:05"), // Go 时间格式化必须用参考时间数字
			Concurrency: s.Concurrency,
			Quality:     s.Quality,
			Status:      s.Status,
			Files:       s.Files,
			Done:        s.Done,
			ElapsedMS:   s.Elapsed.Milliseconds(), // 耗时转毫秒
		})
	}
	c.JSON(http.StatusOK, out)
}

func clampConcurrency(s string) int {
	n, err := strconv.Atoi(s) // 字符串转整数
	if err != nil {
		return minConcurrency // 解析失败用默认值 1
	}
	if n < minConcurrency {
		return minConcurrency // 下限钳位
	}
	if n > maxConcurrency {
		return maxConcurrency // 上限钳位
	}
	return n
}

func clampQuality(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return processor.DefaultQuality // 解析失败用默认质量 80
	}
	if n < minQuality {
		return minQuality // 下限 1
	}
	if n > maxQuality {
		return maxQuality // 上限 100
	}
	return n
}

// fileJSON 是单文件的 JSON 视图（struct tag 控制字段名和 omitempty）
type fileJSON struct {
	OrigName string `json:"orig_name"`                     // 原始文件名
	Size     int64  `json:"size"`                          // 原始大小
	Status   string `json:"status"`                        // 文件状态
	OutName  string `json:"out_name,omitempty"`            // 输出名（空则不序列化）
	OutSize  int64  `json:"out_size,omitempty"`            // 输出大小（0 则不序列化）
	Reason   string `json:"reason,omitempty"`              // 失败原因（空则不序列化）
}

// taskJSON 是任务详情的 JSON 视图
type taskJSON struct {
	ID          string     `json:"id"`                     // 任务 ID
	Status      string     `json:"status"`                 // 任务状态
	DirName     string     `json:"dir_name"`               // 输出目录名
	Concurrency int        `json:"concurrency"`            // 并发数
	Quality     int        `json:"quality"`                // JPEG 质量
	Done        int        `json:"done"`                   // 已完成文件数
	Total       int        `json:"total"`                  // 文件总数
	ElapsedMS   int64      `json:"elapsed_ms,omitempty"`   // 耗时毫秒（0 则不序列化）
	Files       []fileJSON `json:"files"`                  // 文件列表
}

// estimateFileJSON 是压缩预览的单文件 JSON 视图
type estimateFileJSON struct {
	OrigName string `json:"orig_name"`                     // 原始文件名
	Size     int64  `json:"size"`                          // 原始大小
	Est      int64  `json:"est"`                           // 预估压缩后大小
	Err      string `json:"err,omitempty"`                 // 错误信息（空则不序列化）
}

// historyJSON 是历史列表的 JSON 视图
type historyJSON struct {
	ID          string `json:"id"`                         // 任务 ID
	CreatedAt   string `json:"created_at"`                 // 创建时间字符串
	Concurrency int    `json:"concurrency"`                // 并发数
	Quality     int    `json:"quality"`                    // JPEG 质量
	Status      string `json:"status"`                     // 任务状态
	Files       int    `json:"files"`                      // 文件总数
	Done        int    `json:"done"`                       // 已完成数
	ElapsedMS   int64  `json:"elapsed_ms"`                 // 耗时毫秒
}

func toTaskJSON(v store.TaskView) taskJSON {
	files := make([]fileJSON, 0, len(v.Files)) // 预分配容量
	for _, f := range v.Files { // 把 store.FileItem 转成 fileJSON
		files = append(files, fileJSON{
			OrigName: f.OrigName,
			Size:     f.Size,
			Status:   f.Status,
			OutName:  f.OutName,
			OutSize:  f.OutSize,
			Reason:   f.Reason,
		})
	}
	return taskJSON{
		ID:          v.ID,
		Status:      v.Status,
		DirName:     v.DirName,
		Concurrency: v.Concurrency,
		Quality:     v.Quality,
		Done:        int(v.Done),                 // int64 转 int
		Total:       len(v.Files),               // 文件总数
		ElapsedMS:   v.Elapsed.Milliseconds(),   // 耗时转毫秒
		Files:       files,
	}
}