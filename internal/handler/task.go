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
	minQuality     = 50
	maxQuality     = 95
)

type Handler struct {
	svc *service.Service
}

func New(svc *service.Service) *gin.Engine {
	h := &Handler{svc: svc}
	r := gin.New()
	r.Use(gin.Recovery())
	r.POST("/api/tasks", h.createTask)
	r.GET("/api/tasks/:id", h.getTask)
	r.GET("/api/tasks/:id/events", h.events)
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

	view, err := h.svc.CreateTask(fhs, clampConcurrency(c.PostForm("concurrency")), clampQuality(c.PostForm("quality")))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 上传即开跑：worker 池异步处理，接口立即返回任务快照
	go func() { _ = h.svc.RunByID(view.ID) }()
	c.JSON(http.StatusOK, toTaskJSON(view))
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
	if _, ok := h.svc.Task(id); !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "任务不存在"})
		return
	}

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="fileforge-%s.zip"`, id))

	zw := zip.NewWriter(c.Writer)
	for _, f := range h.svc.DoneFiles(id) {
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
	Concurrency int        `json:"concurrency"`
	Quality     int        `json:"quality"`
	Done        int        `json:"done"`
	Total       int        `json:"total"`
	ElapsedMS   int64      `json:"elapsed_ms,omitempty"`
	Files       []fileJSON `json:"files"`
}

type historyJSON struct {
	ID          string `json:"id"`
	CreatedAt   string `json:"created_at"`
	Concurrency int    `json:"concurrency"`
	Quality     int    `json:"quality"`
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
		Concurrency: v.Concurrency,
		Quality:     v.Quality,
		Done:        int(v.Done),
		Total:       len(v.Files),
		ElapsedMS:   v.Elapsed.Milliseconds(),
		Files:       files,
	}
}
