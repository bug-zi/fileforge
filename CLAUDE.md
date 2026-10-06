# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目状态

FileForge（文件工坊）——完全本地运行的批量图片处理工具（Go + Gin，单文件 exe）。**阶段 1~5 已全部落地并通过浏览器实测**（骨架 → 处理核 → worker 池/zip → SSE/质量可选 → 历史对比表/优雅停机/README/答辩材料）；此后按开发者需求迭代（**design v0.3 草案已实现：两段式「先预览后转换」、精确压缩预览、outputs 时间命名**）。需求正本在 `docs/project/fileforge/design.md`（**v0.3 草案，待开发者审核定稿**），设计全记录在同目录 `2026-10-06-fileforge-design.md`；`doc/` 是旧文档库快照，**只读保留，永不改动**。开发以 `design.md` 定稿为准；实现与文档冲突时先与用户确认。

- 项目性质：Go 语言学习验收项目，验收形式为现场演示（并发 1 vs 16 耗时对比是核心卖点）+ 文档答辩
- 硬性要求：全部功能打包进单个 exe（前端经 go:embed 嵌入），运行时零外部依赖
- 功能范围：批量压缩（最长边 ≤1920 + JPEG 质量可选 1~100 全区间，默认 80）、MD5 去重、统一重命名、SSE 实时进度、并发数可调（1~32）、zip 流式下载

## 文档体系与维护权责（最重要）

`docs/` 目录是开发者与 AI 共同维护的项目文档库，权责划分如下：

| 内容 | 维护方 | AI 的角色 |
|---|---|---|
| docs/project/fileforge/design.md | 开发者主导 | 可提建议；经开发者审核通过才可修改，**不得擅自改写** |
| docs/project/fileforge/designs-specs.md | **AI 生成并维护** | 基于 design.md 严格生成，是开发的直接依据；开发者仅简单审核 |
| docs/project/project.md | 开发者主导 | 可建议；协助完善需审核通过 |
| docs/log/YYMMDD.md | AI | 每天一个开发日志；**每个小节标题下必须附创建时间行**（`> 创建于 YYYY-MM-DD HH:MM`，用 date 取实际时间） |
| docs/project/idea/ | **开发者** | AI 不需要关注、不得修改（例外：AI 产出的 spec/plan 按机制存放于此，只动自己产出的文件） |
| docs/project/README.md | **AI** | spec/plan 存放与归档机制正本 |
| docs/project/ 内各模块 spec/plan 文档 | **AI** | brainstorming/写计划产出，按模块就地存放；落地或废弃后移入所属目录 archive/（机制见 README.md） |
| docs/project/新功能开发区.md | **开发者** | AI 据此 brainstorming 立项并实施；完成后条目勾选并按轮归档 |
| docs/project/优化建议区.md | **开发者** | AI 据此实施优化；实施后按轮归档进归档区 |
| docs/project/问题疑惑区.md | **开发者** | **AI 不要主动查阅**，开发者指明时才阅读并答疑 |
| docs/project/临时草稿（待写入）.md | **开发者** | **AI 不需要读取**，建后不再读取/修改 |
| 各子目录同名说明 md（log.md / fileforge.md 等） | 随目录权责 | 目录功能与用法说明；新建子目录时随建随补 |

工作流程：开发者写 design.md → AI 据此生成 designs-specs.md → AI 按 designs-specs.md 开发。

## 常用命令

模块尚未初始化，首次开发前需 `go mod init fileforge`。

```
go build -o fileforge.exe .   # 构建单文件 exe（验收要求可运行）
go vet ./...                  # 静态检查（落地判定之一）
go test ./...                 # 全部测试（落地判定之一）
go test -race ./...           # 竞态检测（store 并发读写必须过）
go test ./internal/processor/ -run TestXxx -v   # 运行单个测试
```

落地判定：`go vet` + `go test` 全绿 + `go build` 产出可运行 exe。

第三方依赖仅允许 1 个：`golang.org/x/image/draw`（高质量缩放）。其余（图片编解码、zip、HTTP）一律用标准库 + Gin。

## 架构

四层结构，依赖方向自上而下，严格单向：

```
main.go（装配 + 启动 + 优雅停机）
  └─ internal/handler/    HTTP 层：multipart 上传 / SSE 进度 / zip 下载 / 静态页
       └─ internal/service/  任务生命周期：建任务、job 分发到 channel、worker 池、结果汇总
            ├─ internal/processor/  单文件处理纯函数：解码→缩放→重编码、MD5、重命名
            └─ internal/store/      内存任务表（sync.Mutex + map），无数据库
  web/  index.html + app.js（原生 JS 单页，经 go:embed 嵌入）
```

核心数据流：上传文件存 `uploads/{taskID}/` → job 写入带缓冲 channel → N 个 worker goroutine 消费 → 输出到 `outputs/{taskID}/` → atomic 计数 + SSE 广播进度 → 完成后 archive/zip 流式打包下载。

关键并发模型（答辩核心，勿随意改设计）：worker 池大小 = 用户传入的并发数；`sync.WaitGroup` 收尾、`atomic.Int64` 计数、worker 内 recover 做单文件错误隔离。

## 有意的设计决策（不要"修复"它们）

- 任务不持久化，存内存，重启即清——有意为之，答辩讲解点
- PNG 统一转 JPEG 输出——换规格统一与体积最优
- 单文件失败不中断整批——失败文件仅记录状态，原样归档不进 zip
- 无前端框架、无构建工具——原生 HTML + JS 单页
- EXIF 方向未处理是已知限制，仅用户明确要求时才加

## 全局硬性规则（用户要求，必须遵守）

1. 前端页面、组件、按键等**禁止使用 emoji 作图标**；需要图标时从 https://fonts.google.com/icons 搜索获取。
2. 组件框、按键、弹窗等**禁止使用彩亮饱和颜色**，须使用与主题色相近的色系，保持整体色彩风格一致。

## Git 操作纪律（开发者指令）

- **禁止 AI 自主执行任何 git 操作**（add / commit / push / pull / checkout / reset 等全部命令）。
- 所有 git 操作由开发者自主完成；AI 最多在开发者明确要求时给出命令建议，不得代为执行。
