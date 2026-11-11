# MVP 验证记录

## 交付范围

本地 Go CLI，在原生 Codex CLI 与 Claude Code CLI 之间接续单个逻辑对话。
提供新建、接入、恢复、双向切换、对话列表、完整上下文分页与环境诊断。
macOS 完成运行验证，Linux amd64 完成交叉构建。数据与原生会话映射持久化在本机。

## 分工与整合

- Terra 实现对话存储、交接包；DeepSeek 独立验证核心。
- DeepSeek 实现两个原生适配器；Terra 独立验证适配器，DeepSeek 修复完整性问题。
- GLM CLI 草稿出现重复调试，按用户要求取消，保留草稿。
- DeepSeek 接续完成 CLI 重构、终端继承、回执恢复、固定后端与集成测试。
- Terra 独立验收完整 CLI，主线程合入其回归测试并修复最终三项发现。

## 已修复的验收发现

1. 大工具输出不能挤掉最新用户要求；小预算保留有效摘录；编码失败明确报错。
2. app-server 不完整响应不能伪装成空历史，分页达到上限明确失败。
3. 同轮双表示去重不删除后续同文本用户请求；可对应的原生 item ID 保持一致。
4. 每个 Codex binding 固定读取后端，避免无法安全映射的旧日志被跨后端重复导入。
5. 换行结束的损坏记录报错；无关 cwd 的 Claude transcript 不阻塞当前目录。
6. app-server 通知队列阻塞时关闭进程可释放读取协程。
7. 显式恢复另一个 agent 会同步新增上下文并更新 ActiveAgent。
8. 对话锁覆盖准备、原生进程运行和结算；嵌套结算复用锁，等待锁可取消。
9. Terminus 单独收到 SIGTERM 时取消前台 native 子进程；菜单等待也能有界退出。

## 自动化检查

以下检查在最终生产代码修复后通过：

```sh
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
go build -o bin/terminus ./cmd/terminus
GOOS=linux GOARCH=amd64 go build -o bin/terminus-linux-amd64 ./cmd/terminus
python3 scripts/smoke_pty.py ./bin/terminus
git diff --check
```

集成测试使用隔离的原生历史目录和合成 Go 可执行文件，驱动实际适配器、存储与
交接代码。覆盖两个方向和多次往返、native session ID/cwd、目标增量、首请求
去重、后续真实重复请求保留、工具输出、pending 恢复、已写回执但进程退出非零、
无回执重试、多候选拒绝、配置/来源记录只读和异常格式。

独立新增的活动 agent 与锁回归已通过。SIGTERM 回归检查父进程退出码 143，并
验证合成 native 子 PID 已退出；菜单输入阻塞时也能终止。

PTY smoke 使用操作系统实际伪终端，确认 native stdin/stdout/stderr 均为 TTY，
可进入 raw mode、读取一个未换行字符，恢复后回到 Terminus 菜单，绑定唯一回执，
保留用户请求和回复并正常退出。脚本可以重复运行，临时数据在结束时删除。

## 验证边界

- 双向对话使用合成 native CLI 验证，未运行真实模型对话；真实终端 UI 与
  原生日志的所有未来版本不能由这些测试保证。
- Codex app-server 协议按官方文档及本机 schema 编写，未验证真实新线程首次
  出现在索引中的时序；缺失回执时保留 pending 并报错，后续操作可重试。
- 某些旧 rollout 无原生 item ID，多文本块也可能无法跨读取后端一一对应，
  因此 binding 固定后端，不能无声迁移。
- Claude JSONL 是内部格式；已知元数据被过滤，未知对话记录和实际损坏会报错。
- 长对话的注入受预算约束，完整历史通过 `terminus context` 获取；应将二进制
  放入 PATH，让 native agent 能运行该命令。
- SIGTERM 检查覆盖 Terminus 自己启动的直接前台子进程；没有验证任意独立后台
  任务或全部第三方后代进程的清理行为。
- Linux 已构建但未在 Linux 机器执行；Windows 不在本期范围。

本期不包含长期记忆、独立任务管理、跨机器同步和全局检索。
