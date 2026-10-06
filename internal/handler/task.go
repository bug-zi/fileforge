package handler

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"fileforge/internal/processor"
	"fileforge/internal/service"
	"fileforge/internal/store"
)

const (
	minConcurrency = 1
	maxConcurrency = 32
	minQuality     = 1
	maxQuality     = 100
)

type Handler struct {
	svc *service.Service
}

func New(svc *service.Service) *gin.Engine {
	h := &Handler{svc: svc}
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/api/tasks", h.createTask)
	r.POST("/api/tasks/:id/start", h.startTask)
	r.GET("/api/tasks/:id", h.getTask)
	r.GET("/api/tasks/:id/events", h.events)
	r.GET("/api/tasks/:id/estimate", h.estimate)
	r.GET("/api/tasks/:id/download", h.download)
	r.GET("/api/history", h.history)
	return r
}

func (h *Handler) createTask(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 multipart 表单"})
		return
	}
	fhs := form.File["files[]"]
	if len(fhs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未收到任何文件"})
		return
	}

	// 只入库不处理：任务进入待确认，预览与开始转换走后续接口
	view, err := h.svc.CreateTask(fhs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, toTaskJSON(view))
}

// startTask 开始转换：写入并发数与质量后异步执行 worker 池。
func (h *Handler) startTask(c *gin.Context) {
	id := c.Param("id")
	if _, ok := h.svc.Task(id); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	view, err := h.svc.Reserve(id, clampConcurrency(c.PostForm("concurrency")), clampQuality(c.PostForm("quality")))
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	go func() { _ = h.svc.RunByID(id) }()
	c.JSON(http.StatusOK, toTaskJSON(view))
}

// estimate 返回待确认任务在指定质量下的精确输出大小预览。
func (h *Handler) estimate(c *gin.Context) {
	id := c.Param("id")
	view, ok := h.svc.Task(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	if view.Status != store.TaskReady {
		c.JSON(http.StatusConflict, gin.H{"error": "任务已开始或已完成"})
		return
	}
	quality := clampQuality(c.Query("quality"))
	files, total, err := h.svc.Estimate(id, quality)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	out := make([]estimateFileJSON, 0, len(files))
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

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")

	ch, cancel, _ := h.svc.Subscribe(id)
	if cancel != nil {
		defer cancel()
	}

	writeFrame := func(payload []byte, final bool) {
		if final {
			c.Writer.WriteString("event: complete\ndata: ")
		} else {
			c.Writer.WriteString("data: ")
		}
		c.Writer.Write(payload)
		c.Writer.WriteString("\n\n")
		c.Writer.Flush()
	}

	// 订阅时任务已完成（小任务可能比建连更快）：立即补发 final
	if view.Status == store.TaskDone {
		final, _ := json.Marshal(map[string]any{"done": len(view.Files), "total": len(view.Files), "final": true})
		writeFrame(final, true)
		return
	}

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case payload, open := <-ch:
			if !open {
				return
			}
			final := strings.Contains(string(payload), `"final":true`)
			writeFrame(payload, final)
			if final {
				return
			}
		}
	}
}

func (h *Handler) getTask(c *gin.Context) {
	view, ok := h.svc.Task(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}
	c.JSON(http.StatusOK, toTaskJSON(view))
}

func (h *Handler) download(c *gin.Context) {
	id := c.Param("id")
	view, ok := h.svc.Task(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="fileforge-%s.zip"`, view.DirName))

	zw := zip.NewWriter(c.Writer)
	for _, f := range h.svc.DoneFiles(view.DirName) {
		w, err := zw.Create(f.Name)
		if err != nil {
			continue
		}
		src, err := os.Open(f.Path)
		if err != nil {
			continue
		}
		_, _ = io.Copy(w, src)
		src.Close()
	}
	zw.Close()
}

func (h *Handler) history(c *gin.Context) {
	sums := h.svc.ListHistory()
	out := make([]historyJSON, 0, len(sums))
	for _, s := range sums {
		out = append(out, historyJSON{
			ID:          s.ID,
			CreatedAt:   s.CreatedAt.Format("2006-01-02 15:04:05"),
			Concurrency: s.Concurrency,
			Quality:     s.Quality,
			Status:      s.Status,
			Files:       s.Files,
			Done:        s.Done,
			ElapsedMS:   s.Elapsed.Milliseconds(),
		})
	}
	c.JSON(http.StatusOK, out)
}

func clampConcurrency(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return minConcurrency
	}
	if n < minConcurrency {
		return minConcurrency
	}
	if n > maxConcurrency {
		return maxConcurrency
	}
	return n
}

func clampQuality(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return processor.DefaultQuality
	}
	if n < minQuality {
		return minQuality
	}
	if n > maxQuality {
		return maxQuality
	}
	return n
}

type fileJSON struct {
	OrigName string `json:"orig_name"`
	Size     int64  `json:"size"`
	Status   string `json:"status"`
	OutName  string `json:"out_name,omitempty"`
	OutSize  int64  `json:"out_size,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type taskJSON struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	DirName     string     `json:"dir_name"`
	Concurrency int        `json:"concurrency"`
	Quality     int        `json:"quality"`
	Done        int        `json:"done"`
	Total       int        `json:"total"`
	ElapsedMS   int64      `json:"elapsed_ms,omitempty"`
	Files       []fileJSON `json:"files"`
}

type estimateFileJSON struct {
	OrigName string `json:"orig_name"`
	Size     int64  `json:"size"`
	Est      int64  `json:"est"`
	Err      string `json:"err,omitempty"`
}

type historyJSON struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`
	Concurrency int    `json:"concurrency"`
	Quality     int    `json:"quality"`
	Status      string `json:"status"`
	Files       int    `json:"files"`
	Done        int    `json:"done"`
	ElapsedMS   int64  `json:"elapsed_ms"`
}

func toTaskJSON(v store.TaskView) taskJSON {
	files := make([]fileJSON, 0, len(v.Files))
	for _, f := range v.Files {
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
		Done:        int(v.Done),
		Total:       len(v.Files),
		ElapsedMS:   v.Elapsed.Milliseconds(),
		Files:       files,
	}
}
