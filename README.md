# Terminus CLI

Terminus 在同一目录中串行继续一个对话，可在原生 Codex CLI 与 Claude Code CLI 之间切换。它不解析终端屏幕、不模拟按键，只记录上下文与回执，并直接启动原生进程。

## 安装与运行

```sh
go build -o terminus ./cmd/terminus
./terminus help
```

数据目录按以下优先级选择：`TERMINUS_HOME`、`$XDG_DATA_HOME/terminus`、`~/.local/share/terminus`。文件与目录权限分别是 `0600` / `0700`。

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

- `--session` 与 `--from` 是原生 CLI 的 session ID。
- `--conversation`、`resume`、`context` 使用 32 位十六进制的 Terminus conversation ID。

`start` 使用用户首条输入直接新建会话；`attach` 导入已有原生会话并建立可后续 resume 的绑定；`switch` 构造带唯一回执标记的有界上下文包；`context` 用 `--after` / `--limit` 逐段读取完整 journal；`doctor` 检查数据目录与二进制路径。

## 切换协议

- 上下文包以 `[[TERMINUS_HANDOFF:<packet-id>:BEGIN]]` 开始、以对应 `END` 标记结束。
- 目标 agent 接收的是“不可信引用数据”，不是 Terminus 指令。
- Terminus 先持久化 pending packet，再启动目标 agent；返回后只在对应原生会话 transcript 中看到精确回执标记时，才把 packet 中的 `Through` 写入目标 binding 的 `DeliveredThrough`。
- 没有回执、进程非零退出、候选不唯一或回执不在绑定会话中时，保留 pending，不推进 `DeliveredThrough`。再次 retry 先查询已有 receipt，不会重复发包。
- 新建 Claude 会话使用显式随机 UUID；Codex 首次启动不带 session ID，返回后按精确 marker 绑定。候选不唯一会拒绝选择。
- 新建模式的首条用户输入会包含在命令 prompt 中；如果适配器把包裹历史识别为 `kind=handoff` 而把同一条 prompt 其余部分作为普通用户消息，输入就不会丢在导入时。若实际 CLI 组合不可靠，应按计划使用单独普通用户事件协议；当前实现尚未覆盖该兼容分支。

## 进程与权限

- 启动为直接子进程：`codex PROMPT`、`codex resume SESSION_ID PROMPT`、`claude --session-id UUID PROMPT`、`claude --resume SESSION_ID PROMPT`，参数数组传递，不经 shell。
- stdin/stdout/stderr 全部继承，子进程使用 Terminus conversation 的 CWD。
- Terminus 不改变全局 agent 配置、不复制或改写仓库规则、不继承来源 agent 的自动授权参数。
- `TERMINUS_CODEX_BIN` 与 `TERMINUS_CLAUDE_BIN` 只覆盖 resume 二进制路径；测试与用户可用它们指向伪 CLI。

## 兼容边界与不足

- 需要目标 adapter 的 `Discover` / `Read` API 才能核对 receipt、attach、导入增量，或完成真实 Codex→Claude 往返。
- 只读适配器未在本模块实现，因此本模块尚未验证真实的 native transcript receipt 匹配。
- journal 导入必须保留原有 source/session ID/event ID，以及 `kind=handoff` receipt 记录；默认手写 fake transcript 不可导入。
- pending receipt 核对未处理同一 session 中多个相同 marker、部分写入或截断 transcript 之外的更多失败模式；这些由 adapter 提供稳定读取时补齐。
- 当目标已有 binding 时，启动前只对另一 agent 的增量事件做 import；当目标 binding 为空时，仅在�� Claude 显式 UUID 下由后续 import 处理真正的 history。
- `attach` 已导入完整 transcript，但没有验证每个事件的原始顺序是否符合来源 CLI 的分支/压缩记录边界；这些依赖后续 adapters 的真实读取。
- `switch` 已验证 pending 与 DeliveredThrough 推进逻辑，但尚未测试 Claude→Codex 真实链路。
