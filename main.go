package main

import (
	"context"   // context 上下文（信号取消、超时）
	"embed"     // go:embed 编译期嵌入文件
	"errors"    // errors.Is 错误比较
	"io/fs"     // fs.Sub / fs.ReadFile 操作嵌入的 FS
	"log"       // 日志输出
	"net/http"  // http.Server 启动与优雅停机
	"os"        // os.Interrupt 信号
	"os/signal" // signal.NotifyContext 监听信号
	"syscall"   // syscall.SIGTERM 信号
	"time"      // 10 秒停机超时

	"github.com/gin-gonic/gin" // Web 框架

	"fileforge/internal/handler" // HTTP 层
	"fileforge/internal/service" // 业务层
	"fileforge/internal/store"   // 数据层
)

// go:embed 是编译指令，把 web/ 目录塞进二进制（单 exe 零依赖的关键）。
// 注意：指令行后面不能跟注释，否则 Go 会把注释当作模式解析。
//
//go:embed web
var webFiles embed.FS

func main() {
	// 阶段 1：装配组件（依赖注入，严格单向：store → service → handler）
	st := store.New()                            // 数据层：内存任务表
	svc := service.New(st, "uploads", "outputs") // 业务层：依赖 store + 两个目录
	r := handler.New(svc)                        // HTTP 层：依赖 service

	// 阶段 2：挂载前端静态资源
	sub, err := fs.Sub(webFiles, "web") // 从嵌入的 FS 里取 web 子目录（相当于 cd web）
	if err != nil {
		log.Fatal(err)
	}
	indexHTML, err := fs.ReadFile(sub, "index.html") // 读首页内容
	if err != nil {
		log.Fatal(err)
	}
	r.GET("/", func(c *gin.Context) { // 首页路由：直接返回 index.html
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
	r.StaticFS("/static", http.FS(sub)) // 静态资源路由：http.FS 把 fs.FS 适配成 http.FileSystem

	// 阶段 3：启动 HTTP 服务
	srv := &http.Server{Addr: ":8080", Handler: r} // 手动构造 Server，为了后面能调 Shutdown

	// 优雅停机：监听 SIGINT（Ctrl+C）和 SIGTERM（kill），收到后 ctx 被取消
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() { // HTTP 服务放 goroutine 跑，主协程要等信号
		log.Println("FileForge 已启动，请打开 http://localhost:8080")
		// ListenAndServe 阻塞；Shutdown 会让它返回 http.ErrServerClosed（正常关停），其他错误才 Fatal
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	// 阶段 4：优雅停机
	<-ctx.Done() // 主协程阻塞在这里，直到收到退出信号
	log.Println("收到退出信号，等待在途请求完成…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // 最多等 10 秒
	defer cancel()
	// Shutdown：停止接新连接，等正在处理的请求完成；超时则强制关闭
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("停机超时，强制退出: %v", err)
	}
	log.Println("FileForge 已退出")
}
