# fileforge

FileForge 应用主体模块目录：存放该模块的需求正本（`design.md`）、AI 开发依据（`designs-specs.md`）、brainstorming 设计记录与实施计划（`YYYY-MM-DD-<主题>-design.md` / `-plan.md`）及归档（`archive/`）。

**使用方式**：需求变更由开发者改 `design.md`；AI 据此同步 `designs-specs.md`；单次功能的设计/计划以日期前缀文件就地存放，落地或废弃后移入 `archive/`（机制见 `docs/project/README.md`）。

**边界**：AI 不得擅改 `design.md`（经开发者审核通过才可修改）；spec/plan 归档只移动、永不删除。
