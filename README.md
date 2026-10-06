# FileForge · 文件工坊

完全本地运行的批量图片处理工具：**单文件 exe、零外部依赖、拖进来就压**。同时解决重复文件（MD5 去重）与文件名混乱（统一重命名）两个搭车痛点。

## 为什么做它

批量压缩图片是高频刚需，但现有工具各有硬伤：

- 在线压缩网站：照片要上传到陌生服务器（隐私风险），常有数量/大小限制、广告、注册门槛；
- 微信/QQ 传输压缩：压缩率不可控，画质损失不可知；
- 桌面工具软件：捆绑全家桶、广告；
- 命令行工具（ImageMagick）：对普通用户有技术门槛。

FileForge 的答案：**文件不出自己的电脑，双击就用，一次管够**。

## 功能

- 浏览器拖入一批图片（jpg/png），**先预览后转换**：上传后进入待确认状态，点「开始转换」才压缩；
- 压缩预览：每文件按当前质量真实编码一次，展示精确的压缩后大小（滑块调整即刷新）；
- 等比压缩：最长边 > 1920 等比缩放，JPEG 重编码；压缩质量可调（1~100 全区间，默认 80；质量 100 重编码可能大于原图，预览如实呈现）；
- MD5 去重：内容相同的文件只保留首个，不进结果包；
- 统一重命名：结果按 `photo_001.jpg` 序列命名；
- 并发数可调（1~32），实时进度条（SSE 推送，断线自动降级轮询）；
- 历史耗时对比表（并发收益一目了然）；
- 一键打包 zip 下载（流式，按任务时间命名如 `fileforge-261007-0110.zip`）；
- 单文件失败不中断整批：失败/重复文件在列表中单独标注。

## 运行

无外部依赖，二选一：

```
双击 fileforge.exe                 # 然后浏览器打开 http://127.0.0.1:8080
go build -o fileforge.exe . && ./fileforge.exe
```

Ctrl+C 优雅退出（等待在途请求完成）。任务存内存，重启即清（有意设计）。

## 架构

```
main.go（装配 + 启动 + 优雅停机）
  └─ internal/handler/    HTTP 层：multipart 上传 / SSE 进度 / zip 下载 / 静态页
       └─ internal/service/  任务生命周期：建任务、job 分发到 channel、worker 池、结果汇总
            ├─ internal/processor/  单文件处理纯函数：解码→缩放→重编码、MD5、重命名
            └─ internal/store/      内存任务表（sync.Mutex + map），无数据库
  web/  index.html + app.js（原生 JS 单页，经 go:embed 嵌入二进制）
```

并发模型：上传即建任务，每个文件包成 job 写入带缓冲 channel，N 个 worker goroutine（N = 并发滑块）消费；`sync.WaitGroup` 收尾、`atomic` 计数与发号、单文件 `recover` 隔离；每个文件终结即时通过 SSE 推送进度。

## 测试

```
go vet ./...          # 静态检查
go test -race ./...   # 全部测试 + 竞态检测
go build -o fileforge.exe .
```

覆盖：processor 压缩/判重/损坏文件、store 并发读写、handler 上传/SSE/下载闭环（`httptest`）。

## 技术要点

- goroutine + channel 任务分发，并发收益可用历史耗时表现场量化（并发 1 vs 32）；
- SSE 实时推送（`text/event-stream`），轮询为降级备份；
- `go:embed` 将前端嵌入单二进制，运行时零外部服务；
- multipart 流式上传、`archive/zip` 流式下载；
- 第三方依赖仅 `golang.org/x/image/draw`（高质量缩放）与 Gin。

## 已知限制（有意取舍）

- 任务不持久化：存内存，重启即清；
- PNG 统一转 JPEG 输出（换规格统一与体积最优）；
- EXIF 方向信息未处理：手机竖拍照片可能方向错误。
