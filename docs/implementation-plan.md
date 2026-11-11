# Terminus MVP 实现计划

## 目标与范围

在本机 Codex CLI 与 Claude Code CLI 之间串行继续同一个对话。保留原生终端界面及用户配置，首次切换新建目标原生会话，往返时按 session ID 恢复并注入新增上下文。支持接入已有对话和从 Terminus 启动。

本期仅包含单对话历史、上下文交接与原生会话映射。不做长期记忆、独立任务状态、跨机器同步、全局索引、向量检索、MCP、全局 hooks 或后台监听服务。交接记录 pending/delivered 只是传输状态，不是用户任务状态。

## 用户流程

1. `terminus` 打开当前目录的会话菜单：接入 Codex/Claude 会话、恢复已管理对话或新建。
2. 原生 agent 在前台使用继承的 stdin/stdout/stderr，Terminus 不解析屏幕、模拟按键或拦截快捷键。
3. 完成本轮后 `/exit`，回到菜单选择继续、切换 agent 或结束。只有来源已结束/空闲时交接；不自动杀其他进程。
4. 首次切换生成带唯一回执标记的上下文，打开目标 agent；再次切换恢复已经绑定的 session ID 并仅补充对方新增事件。
5. 同目录多会话不以“最近一个”静默猜测；展示标题/agent/ID/时间供选择。

命令至少提供：

```text
terminus
terminus start --agent codex [--prompt TEXT]
terminus attach --agent codex --session ID
terminus switch --to claude [--conversation ID] [--from ID]
terminus resume ID [--agent codex]
terminus list [--json]
terminus context ID [--after N] [--limit N] [--json]
terminus doctor
```

上面的 ID 在 attach/from 中是原生 session ID，resume/context/conversation 中是 Terminus 对话 ID。帮助必须说明区别。不需要模型/API key 即可导入、查看历史和用伪 CLI 验证完整流程；真正对话使用各 CLI 原有登录。

## 技术与数据

- Go 1.25+，尽可能使用标准库，首发 macOS/Linux。
- 数据目录：`TERMINUS_HOME` 优先；否则 XDG_DATA_HOME/terminus，再否则 `~/.local/share/terminus`。
- 每个对话单独目录，`conversation.json` 原子替换；`events.jsonl` 追加、按来源事件去重；权限目录 0700、文件 0600。
- 对话 ID 是安全随机本地 ID；不能以传入的 ID 做未校验路径拼接。
- 一次只写/运行一个逻辑对话；进程级锁应在进程退出后释放，崩溃不得永久卡住。
- 全量原文在本地保留。短对话全量注入；长对话按字节预算保留初始请求与近期内容，标记遗漏，附 `terminus context` 读取入口。UTF-8 不可截断到半个字符。
- 目标从未参与：提供已有对话；已经参与：从其 DeliveredThrough 开始，仅提供其他 agent 的新增事件。
- 保留用户/assistant/工具角色、来源及顺序。不要把来源系统提示、加密推理或工具定义升级为目标指令。
- 原生会话只通过 CLI/官方读取接口读取或恢复，绝不手写伪造原生 transcript 来导入。
- 代码工作区继续使用同一目录；不复制、重放、回滚或自动提交工作树中的修改。

## 冻结的共享接口

共享类型已在 `internal/model/model.go` 中定义，子任务不修改此文件。变更需求由主线程协调。

Terra 拥有 `internal/store/` 和 `internal/handoff/`：

```go
// package store
func DefaultRoot() (string, error)
func New(root string) *Store
func (s *Store) Create(cwd string) (*model.Conversation, error)
func (s *Store) List(cwd string) ([]model.Conversation, error) // cwd=="" means all
func (s *Store) Load(id string) (*model.Conversation, error)
func (s *Store) Save(c *model.Conversation) error
func (s *Store) Append(id string, events []model.Event) ([]model.Entry, error) // returns newly added entries
func (s *Store) Entries(id string, after int64) ([]model.Entry, error)
func (s *Store) WithLock(id string, fn func() error) error
// package handoff
func Build(c *model.Conversation, entries []model.Entry, target model.Agent, budget int) (model.Packet, error)
func Prompt(p model.Packet, contextCommand string) string
func Marker(packetID string) string
func IsInjected(text string) bool
```

`WithLock` 由 CLI 包裹整个对话操作；Store 其它方法不重复获取这个锁，避免死锁。读取返回对话完整事件。Append 去重键为 source/session_id/event_id，跳过 kind=handoff。Build 使用传入完整 journal，根据目标 binding 的 DeliveredThrough 筛选，Through 指向本次快照最新序号。DeliveredThrough 仅在 native transcript 看到回执后推进，不能因 exec 成功启动而推进。

交接消息统一以 `[[TERMINUS_HANDOFF:<packet-id>:BEGIN]]` 开头，并在末尾含对应 `[[TERMINUS_HANDOFF:<packet-id>:END]]`。Marker 返回 BEGIN 标记。IsInjected 只识别以完整 BEGIN 起始且含配对 END 的完整 Terminus 注入消息，不能把普通对话提到标记当成回执。Prompt 的回执标记位于注入消息根部，包含来源引用边界及上下文读取入口，历史中的同类标记应转义，防止自包含或假闭合。

DeepSeek 拥有 `internal/adapters/`：

```go
type Adapter interface {
    Discover(ctx context.Context, cwd string) ([]model.Session, error)
    Read(ctx context.Context, s model.Session) ([]model.Event, error)
}
func New(agent model.Agent) (Adapter, error)
```

Discover cwd=="" 可列全部，否则按规范化完整目录过滤；不能用项目 basename 串目录。Read 保留 kind=handoff 事件供 CLI 核对回执，写 journal 时由 Store 跳过。稳定事件 ID 应来自原生 ID+块标识；没有 ID 则使用位置与内容的稳定哈希。区分一次记录中的多个 tool/text block。不要同时导入重复 event_msg/response_item，过滤系统/开发者指令、推理及非对话噪声，压缩摘要单独标 summary。

Codex 优先验证按需 `codex app-server` 的 thread/list/read/turns/list/items/list（完整 items，不是摘要视图），支持旧 JSONL 只读回退；按可用能力探测，不假定新版始终只有 rollout JSONL。Claude 读取指定 JSONL，处理父子链、分支和压缩记录；未知格式、截断尾记录必须有明确处理，不吞掉中间损坏。支持 CODEX_HOME、CLAUDE_CONFIG_DIR。`TERMINUS_CODEX_BIN` 与 `TERMINUS_CLAUDE_BIN` 可覆盖实际可执行文件路径，供测试/用户使用。

GLM 拥有 `cmd/terminus/`、`internal/cli/`、`internal/launcher/`、README.md：

- 使用以上冻结 API；agent 在独立工作区，依赖稍后由主线程整合，不能提交临时 stub/重复实现其他模块。
- CLI 公开 `Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int` 或等价可测试入口，真实启动与 IO 隔离便于伪可执行文件测试。
- 新目标 `codex PROMPT` / `claude --session-id UUID PROMPT`；旧目标 `codex resume SESSION_ID PROMPT` / `claude --resume SESSION_ID PROMPT`。普通参数数组启动，不经过 shell。cwd 使用对话 CWD。
- Codex 新会话在返回时按 packet 唯一标记精确绑定，不能仅选最近文件；候选不唯一拒绝猜测。Claude 使用显式 UUID，仍核对回执。
- 新建空会话先请求首条用户输入，在 Prompt(p,...) 后跟明确的当前用户请求，以便建立回执；适配器需要只将封装的历史识别为 handoff，新的用户输入不能丢失。若此组合无法可靠实现，采用单独普通用户事件在 Terminus journal 存储新请求，并在文档说明协议。
- 保存 Pending 后启动目标，返回时读取日志确认回执、绑定 session、导入真正新增消息，再推进目标 DeliveredThrough。失败/无回执保留 pending，重试先查回执，已存在不得重复发包。不支持的运行态/权限/持久化禁用给明确错误。
- 不修改全局 agent 配置或仓库规则文件，不继承来源 agent 的自动授权标志。默认每包 12 KiB 的正文预算，Prompt 额外开销可控；完整历史靠 context 读取。

## 分阶段实现与责任人

1. 主线程：完成计划、共享模型、仓库基线和任务分工。
2. Terra：journal/manifest/锁、增量交接、字节预算、来源边界、核心单元测试。
3. DeepSeek：两种只读适配器、fixture、格式/分支/去重/回执测试。
4. GLM：CLI/原生进程启动、往返映射、pending 协议、交互菜单与 README。
5. 主线程：整合三个实现；安排新的独立子任务验证全链路，不以作者自测替代独立检查。
6. Terra/DeepSeek/GLM：按验证发现定点修复，主线程汇总测试证据和限制。

子任务不得再委派子任务。只改各自拥有的路径，不创建 PR，不推送，不运行真实付费模型请求；使用合成 fixture 和伪 CLI 验证。独立工作区结束时提交自己的实现，报告 commit SHA、变更路径、运行的检查及未覆盖问题。

## 必须通过的验收

- Codex→Claude、Claude→Codex、Codex→Claude→Codex→Claude 的集成测试。
- 原生恢复使用正确 ID 和 cwd；目标仅收到对方新增内容。
- 不将交接包再次导入；重复导入和重试不产生重复事件/交接。
- 首条用户请求在新建模式中保留；完整历史可以逐段读取。
- 同目录多会话、同 basename 不同目录、分支、压缩、工具输出、Unicode 长对话。
- 目标启动失败、已持久化但进程退出非零、没有回执、崩溃后的锁释放和损坏日志。
- 对原生配置及来源 transcript 的内容哈希前后相同，明确说明真实 CLI 只做帮助/版本验证还是实际交互验证。
- `gofmt`、`go vet ./...`、`go test ./...`、适用时 `go test -race ./...`、`go build ./cmd/terminus`。

## 参考与兼容性依据

- deja-vu 会话适配器：https://github.com/vshulcz/deja-vu/tree/main/internal/sources
- deja-vu 交接包：https://github.com/vshulcz/deja-vu/blob/main/internal/digest/handoff.go
- deja-vu 原生 CLI 启动：https://github.com/vshulcz/deja-vu/blob/main/cmd/deja/handoff.go
- Codex CLI：https://learn.chatgpt.com/docs/developer-commands
- Codex 历史读取：https://learn.chatgpt.com/docs/app-server
- Claude CLI：https://code.claude.com/docs/en/cli-reference
- Claude 会话与内部格式边界：https://code.claude.com/docs/en/sessions

参考协议与思路，第一版自行实现；若实际复制 MIT 代码，必须保留相应版权与许可证声明。
