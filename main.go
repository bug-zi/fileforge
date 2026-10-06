package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"fileforge/internal/handler"
	"fileforge/internal/service"
	"fileforge/internal/store"
)

//go:embed web
var webFiles embed.FS

func main() {
	st := store.New()
	svc := service.New(st, "uploads", "outputs")
	r := handler.New(svc)

	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Fatal(err)
	}
	indexHTML, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		log.Fatal(err)
	}
	r.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
	r.StaticFS("/static", http.FS(sub))

	srv := &http.Server{Addr: ":8080", Handler: r}

	// 优雅停机：SIGINT/SIGTERM 后停止接新连接，等在途请求做完再退出
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Println("FileForge 已启动，请打开 http://localhost:8080")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，等待在途请求完成…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("停机超时，强制退出: %v", err)
	}
	log.Println("FileForge 已退出")
}
