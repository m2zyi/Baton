# Terminus CLI

Terminus 在同一工作目录中串行继续一个逻辑对话，可在原生 Codex CLI 与 Claude Code CLI 之间切换。它不解析终端屏幕、不模拟按键、不启动后台监听，只负责：

- 读取两个原生 CLI 的只读会话历史；
- 生成带唯一回执标记的有界上下文包；
- 直接启动原生进程并继承终端；
- 在原生日志确认回执后绑定 session、导入新增内容并推进交付游标。

首发支持 macOS 与 Linux。

## 构建与安装

```sh
go build -o terminus ./cmd/terminus
./terminus help
```

不需要模型 API key 即可导入、查看历史，或用伪 CLI 验证完整流程；真正对话沿用各 CLI 原有登录。

## 数据目录与权限

数据目录按以下顺序选择：

1. `TERMINUS_HOME`
2. `$XDG_DATA_HOME/terminus`
3. `~/.local/share/terminus`

每个对话使用独立目录，`conversation.json` 原子替换，`events.jsonl` 追加写入。目录权限为 `0700`，文件权限为 `0600`。进程级锁在进程退出后释放，崩溃不会永久卡住对话。

Terminus 不修改 Codex/Claude 的全局配置、仓库规则或来源 transcript。

## 命令

```sh
terminus
terminus start --agent codex|claude [--prompt TEXT]
terminus attach --agent codex|claude --session NATIVE_SESSION_ID
terminus switch --to codex|claude [--conversation TERMINUS_ID] [--from NATIVE_SESSION_ID]
terminus resume TERMINUS_ID [--agent codex|claude]
terminus list [--json]
terminus context TERMINUS_ID [--after N] [--limit N] [--json]
terminus doctor
terminus help | version
```

ID 区别：

- `--session`、`--from` 是原生 CLI 的 session ID。
- `--conversation`、`resume`、`context` 使用 32 位小写十六进制 Terminus conversation ID。

`resume` 与 `context` 的选项可以放在 ID 之后，例如：

```sh
terminus context 0123abcd0123abcd0123abcd0123abcd --after 20 --limit 10 --json
```

### 菜单

直接运行 `terminus` 打开当前目录的菜单。即使还没有任何已管理对话，菜单也提供：

- 新建 Codex/Claude 对话；
- 接入当前目录已存在的原生会话；
- 打开已有 Terminus 对话；
- 退出。

原生 agent 结束后返回菜单，可以继续同一 agent、切换 agent、查看上下文或退出。同一目录有多个对话时会列出 ID、agent、更新时间和 pending 状态供选择，不会静默选择“最近一个”。

## 交接协议

每次切换生成 `Packet` 并先持久化为 `Pending`，然后启动目标 agent：

- 上下文包以 `[[TERMINUS_HANDOFF:<packet-id>:BEGIN]]` 开始，以对应 `END` 标记结束。
- 包内历史标记会被转义，避免自包含或假闭合。
- 历史被明确标记为不可信引用数据；来源系统提示、推理和工具定义不会升级为目标指令。
- 短对话全量注入；长对话按字节预算保留初始请求、近期内容和工具输出，并给出 `terminus context <id>` 读取入口。截断按 UTF-8 字符边界进行。

目标从未参与对话时，提供已有历史；目标已经参与时，从该目标 binding 的 `DeliveredThrough` 开始，只提供其他来源的新增事件。

返回后，Terminus 只认目标原生 transcript 中完整、配对的精确回执标记：

- 找到唯一回执后，先导入目标新增事件，再把 `Packet.Through` 写入目标 binding 的 `DeliveredThrough`，最后清除 pending。
- 目标进程退出非零但已写入回执时，仍会完成绑定与导入；后续操作不会重复发送该包。
- 没有回执时保留 pending 和非零退出码；重试会先检查已有回执，只有确实缺失时才重发同一包。
- 多个候选 session 含同一标记、回执不在已绑定 session 中、未知后端或适配器读取错误都会明确失败，不猜测。
- `kind=handoff` 回执不会写入 journal，重复注入不会产生重复事件。

新建模式的首条用户请求先以本地 provisional 事件持久化，并作为当前请求附在 `Prompt` 之后。目标 transcript 导入时只跳过与这条 provisional 请求对应的首个原生用户消息，因此首次请求不会因“双写”重复；后续真实重复请求仍会保留。

### 原生启动命令

```text
codex PROMPT
codex resume SESSION_ID PROMPT
claude --session-id UUID PROMPT
claude --resume SESSION_ID PROMPT
```

- 参数数组直接传给 `exec.Cmd`，不经过 shell。
- 新 Codex 会话不带 session ID，返回后按唯一回执精确绑定。
- 新 Claude 会话使用显式随机 UUID，返回后仍核对回执。
- 恢复时使用对话 binding 中的原生 session ID 与对话 CWD。
- `resume` 没有新提示词时不传空 prompt 参数。

`TERMINUS_CODEX_BIN` 与 `TERMINUS_CLAUDE_BIN` 可覆盖实际可执行文件路径；只影响 Terminus 启动与只读适配器，不修改用户配置。

## 原生终端继承

Terminus 将原始的 stdin/stdout/stderr 直接交给 `exec.Cmd`。当它们是 `*os.File` 时，子进程获得同一文件描述符（dup2），而不是 Terminus 建立的 pipe，因此原生 TUI 的 raw mode、窗口大小和快捷键保持有效。

菜单每次都从原 reader 逐字节读取到换行，不使用持久 `bufio.Reader` 预读，避免吃掉随后要交给 native 的输入。取消 context 会终止前台子进程；正常退出后返回菜单。

## 只读后端

Codex：

- 优先使用 `codex app-server` 的 `thread/list`、`thread/read`、`thread/items/list` 或 `thread/turns/list`（`itemsView=full`）。
- 能力不支持时回退到 `$CODEX_HOME/sessions` 下的 JSONL rollout。
- 首次成功读取后，实际后端写入 `model.Session.Backend`（`app-server` 或 `rollout`）。已绑定 session 不会在后续读取中静默切换后端。
- rollout 会话在同一 native session ID 出现更新文件时会跟随新的 JSONL 路径，但仍固定使用 rollout 后端。
- 旧 manifest 没有 `Backend` 时仍可读取：会探测一次并保持兼容。

Claude：

- 读取 `$CLAUDE_CONFIG_DIR/projects` 下的 JSONL，保留父子链、分支选择和压缩摘要。
- 只完整解析 transcript 自身 `cwd` 与目标目录一致的会话。无关 cwd 的损坏或非对话 metadata 不会阻塞目标目录；目标会话自身的截断/损坏仍会报错。
- `api-request-blob` 等已知非对话 metadata 记录被归类忽略；未知且带对话 message 的记录类型会明确报错。

两个适配器都保留稳定事件 ID，区分同一记录中的多个 text/tool block，过滤系统/开发者指令、推理和非对话噪声，并只读取、不写入原生日志。

## doctor

`terminus doctor` 实际检查：

- 数据目录可创建、可写，是目录，并能创建 `0600` 写探针；过宽的目录权限会被收紧到 `0700`。
- Codex 与 Claude 可执行文件可解析（含 `TERMINUS_*_BIN` 覆盖）。

任一检查失败时返回非零。

## 当前边界

- 只支持单逻辑对话；不做长期记忆、跨机器同步、全局索引、向量检索、MCP 或后台服务。
- 不自动杀其他 native 进程，不解析屏幕，不模拟按键。
- 同一 Codex binding 不会在 app-server 与 rollout 之间静默切换；如需切换后端，需要建立新绑定。
- 部分旧日志或多 text block 的原生 ID 可能无法跨后端迁移；已绑定后固定后端避免混用。
- 来源 transcript 的中间损坏、未知对话格式、回执候选不唯一都会阻止导入，不会为了“能用”而吞掉错误。

## 测试

```sh
gofmt -w $(find . -name '*.go' -not -path './.git/*')
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/terminus
```

集成测试使用 Go 测试 helper 作为伪 Codex/Claude 可执行文件，写入合成 transcript，并驱动真实 `internal/adapters`、`internal/store` 与 `internal/handoff`。覆盖：

- Codex→Claude→Codex→Claude 多次往返（以及从 Claude 起步的往返）；
- 原生 resume ID、CWD 与增量内容；
- 首次请求、工具输出和用户消息唯一性；
- 新建、菜单新建、菜单接入、空列表菜单；
- pending 恢复、非零退出但回执已保存、无回执重发、多候选拒绝；
- 来源 transcript 未被 Terminus 改写；
- `CODEX_HOME`、`CLAUDE_CONFIG_DIR`、`TERMINUS_HOME` 完全隔离。

测试不会运行真实模型，也不会读取用户私有会话。
