# AGENTS.md

## 【架构】

- 本机只负责编辑代码；构建、测试、运行、git 操作全部在远程主机 `yup-dev` 上执行。
- 代码通过 mutagen 双向实时同步（秒级）：本地 `~/.zcode/workspace/default/<项目>` ⇄ 远程 `/root/code/<项目>`
- `ssh yup-dev` 已免密，非交互命令直接可用。

## 【执行规则】

- 跑任何项目命令（build/test/run/lint）一律：`ssh yup-dev "cd /root/code/<项目> && <命令>"`
- 超过 2 分钟的任务先挂后台（`tmux new -d` 或 `nohup`），再轮询结果。
- 修改代码永远改本地文件，同步自动带到远程；禁止用 ssh + sed/echo 改远程源码。
- 不在本地跑项目构建/测试，运行环境在远程。

## 【同步纪律】

- git 提交、跑测试前先刷盘：`mutagen sync flush <项目名>`。
- 同步忽略：`node_modules`、`dist`、`target`、`.venv`、`pycache`、`.next`、`.DS_Store`、`*.pyc`，依赖在远程按需安装（首次先 `npm install` / `pip install` 等）。
- 不在远程手改源码；不在两端同时改同一文件。

## 【git 规则】

- 所有写操作（add/commit/push/pull/checkout/merge/rebase）只在远程执行，本地只读（log/diff/show）。
- 远程身份已配置 `cnyup` `<75594270+cnyup@users.noreply.github.com>`，GitHub 走 SSH 443，已验证可推送。
- 标准提交：`mutagen sync flush <项目名>` → `ssh yup-dev "cd /root/code/<项目> && git add -A && git commit -m '信息' && git push"`
- push 前若远端有新提交，先 `git pull --rebase`。

## 【项目管理】

- `zsync create <项目名>`（接入新项目，本地目录需已存在）
- `zsync list` / `zsync flush <项目名>` / `zsync remove <项目名>`

## 【禁止】

- 禁止在本地执行 `git commit`/`git push` 等写操作。
- 禁止跑交互式命令（vim、top 等），非交互 ssh 会卡死；确需交互时告知我手动处理。
- 遇到同步冲突文件（`.conflict`）先报告我，不要擅自删除。
