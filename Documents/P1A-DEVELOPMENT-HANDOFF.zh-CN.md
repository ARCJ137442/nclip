# P1a 临时信道开发交接

状态：截至 2026-11-26，P1a 临时信道已完成实现、审阅与本地端到端验证，并已合入 `main`；本文件交接代码位置、验证方式、已知边界与后续批次，供下一位维护者继续工作。

协议与额度规则以 [P1a 信道契约](P1-CHANNEL-CONTRACT.zh-CN.md) 为准；本文件只做导航与交接，不重复契约条文。

## 1. 分支与提交

| 项 | 值 |
| --- | --- |
| 主线 | `main`，HEAD `147439c`（P1a 与账号 v2 已合入） |
| P1a 部署提交 | `e746578`（信道主体与测试） |
| 主要前置 | 账号 v2（`a593525`）、`feat/p1a-channels`（信道主体）、`fix/p1a-g3-*`、`fix/p1a-g4-*`（审阅修正）；均已合并 |
| 依赖 | Go 1.26；原生 HTML/CSS/JS，无前端框架 |

`main` 已包含信道主体、发送端/接收端 UI、ticket 交付、Range 续传、账号模式集成与审阅修正；`e746578` 只追加反向代理缓冲相关的一行响应头与对应测试，`147439c` 追加本交接文档。

## 2. 代码地图

| 位置 | 职责 |
| --- | --- |
| `internal/channels/manager.go` | 信道、席位、上传会话与幂等键的内存状态机与串行化边界 |
| `internal/channels/seats.go` | join / pending_admit / paired / leave / kick / close 生命周期 |
| `internal/channels/content.go` | 内容 init、分块、finalize、abort 与额度快照 |
| `internal/channels/delivery.go` | 交付声明、ticket、representation 锁定与 Range 续传 |
| `handlers/channels.go` | 状态查询、创建、关闭与错误映射 |
| `handlers/channel_seats.go` | 加入、放行、踢出、心跳、离开 |
| `handlers/channel_content.go` | 内容上传、下载与中止 |
| `handlers/channel_ticket.go` | 交付票据签发与票据下载 |
| `handlers/channel_delivery.go` | app-multipart / browser-attachment 两种表示与响应头 |
| `handlers/channel_response_cleanup.go` | `.nclip-channel-response-` 临时响应目录与租约清理 |
| `static/channel-workspace.js` | 发送端工作区、内容发送与发送端控制 |
| `static/channel-receiver.js` | 接收端加入、等待、下载与终态 |
| `static/channel-session.js` | 会话与 CSRF 头、幂等键（base64url） |
| `static/channel-transfer-path.js` | 交付路径决策（app-multipart / native） |

测试：

| 类型 | 文件 |
| --- | --- |
| 单元 | `internal/channels/*_test.go` |
| 处理器 | `handlers/channel_*_test.go` |
| 主程序/路由 | `main_channels_test.go`、`main_channel_ticket_accounts_test.go`、`main_channel_rejection_test.go`、`main_channel_firstread_test.go` |
| 前端逻辑 | `scripts/transfer.test.mjs`、`scripts/accounts.test.mjs` |

## 3. 必须保持的不变量

- 账号模式下只允许一个持久文件系统写者；进程启动先取得跨进程写者锁，再清理未完成 finalize、信道 staging 与 ephemeral paste。硬崩溃后锁需要人工确认无写者再处理，优雅停止会释放。
- 信道内容只在接收席位与有效 claim 绑定时可读；撤回、kick、close、leave、页面关闭都必须取消在途写出并销毁临时内容。
- 交付票据是路径能力，不得写入访问日志或错误日志；反向代理对票据路径必须关闭访问日志。
- app-multipart 与 browser-attachment 表示一旦锁定，不得中途切换；Range/If-Range 必须在同一表示内续传。
- 单信道同一时刻只有一份待交付内容；额度在 init 时快照，后续管理员调整不影响已创建的会话。
- 信道没有产品 TTL；发送者标签页仍在时信道保持，发送者离开后不再接受新加入。

## 4. 验证方式

- `go test -race ./...`，以及 `node --test scripts/clipboard.test.mjs scripts/accounts.test.mjs scripts/transfer.test.mjs`。
- 浏览器端到端使用隔离数据目录与独立端口，不接触任何既有数据；接收端必须使用 `http://localhost:<port>` 以满足 `crypto.subtle` 的安全上下文要求（局域网地址不是安全上下文）。
- 双浏览器验证要使用两个独立 profile（或两个浏览器），不要用同一 profile 的两个标签页模拟双方。
- 验证矩阵：公开匿名 join→放行→交付；私有 + 有下载权限账号零确认；私有 + 无下载权限被拒；公开 + 已登录无下载权限按公开策略接收；撤回/踢出/关闭；暂停与离开；Range 断流续传；40 MiB 以上 native 落盘与 sha256；资源峰值。
- 跨设备联调用独立实例与独立数据目录，sender/receiver 各在一端；幂等键使用 16 字节随机数的 base64url，不要用带连字符的 UUID。

可运行的手工/自动化验证步骤见 [INTEGRATION-TESTS.md](INTEGRATION-TESTS.md)。

## 5. 已知边界

- 接收页前台心跳 5 秒、隐藏页 30 秒；浏览器对隐藏标签页的强化节流或系统休眠会拉长心跳间隔，超过 90 秒无活证据按断网处理并释放席位，返回时显示终态，需要重新加入。这是产品边界，是否放宽需要产品决策。
- 下载管理器（如 IDM）可能接管 native 下载并导致页面侧无法观察落盘结果；干净浏览器 profile 可规避，属于客户端环境。
- 移动端不在本批范围。
- 仓库继承的 golangci-lint 基线为 33 项，见 [P1 Go lint 基线](P1-GOLANGCI-BASELINE.zh-CN.md)；新增代码不得扩大该基线，清理另立批次。
- fork 的 push 不会自动触发 Actions，CI 需要手动 dispatch；基线未放宽。

## 6. 后续批次

1. lint 清理批次：处理 33 项继承基线（gocyclo 28、errcheck 2、staticcheck 2、unconvert 1），不改行为、不放宽规则。
2. UI 优化批次：性能、全流程可见性与文案整理；已完成审计，未实施。
3. 补充验证（运维方已决定暂缓）：kick/close/leave、native 落盘 hash、Range 断点续传、隐藏超过 5 分钟的策略，在真实部署上各跑一遍。
4. 隐藏标签页释放策略：保持现状、延长窗口、可见性感知心跳或提供"重新加入"入口，四选一需要产品决定。

## 7. 部署与回滚注意事项

- 部署前必须准备数据备份、精确源码镜像与可立即启动的回滚容器；升级账号库版本的镜像不能只回滚容器。
- vhost 需要对票据路径关闭访问/错误日志，并对信道与分享路径关闭请求/响应缓冲与临时文件，否则大文件与 Range 会退化。
- 容器内存上限按 128 MiB 验证过 64 MiB 载荷（峰值约 50 MiB）；临时磁盘峰值约为载荷两倍。
- 回滚后要确认旧镜像仍能读当前账号库版本；不兼容时先恢复对应数据备份。
- 实际主机、域名、路径、镜像 ID、备份位置与验收证据只记录在仓库外的私有运维记录中，不进入本仓库、PR、Issue 或 Actions 日志。
