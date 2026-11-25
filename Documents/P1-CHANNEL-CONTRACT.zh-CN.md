# P1a-0 信道契约冻结（修订审阅稿）

状态：P1a-0 已通过；G1/G2 已通过，G3 实施中；本文不授权部署。

依据：P1 channel design、P1 iteration plan、P0 既有行为及已确认的用户裁定。

## 1. 内容额度与磁盘空间

### 1.1 单信道额度

- 一个信道同一时刻最多有一份待交付内容；该内容逻辑字节总量沿用普通分享的完整 `effectiveUploadLimit(c)` 规则，不另设面向用户的信道额度。
- 内容上传 init 时，服务端按当时的认证用户、服务端 BufferSize、共享上传限制和现有条件计算并固定一次有效额度快照。init 同时验证 manifest 声明的总 part 字节数不超过该快照；每个分块及 finalize 都按同一快照验证。管理员之后调整额度不改变已创建上传会话的额度。
- 此额度限制内容大小，不代表速率或带宽保证。无账号模式继续使用现有 UploadAuth 与服务端上传规则。

### 1.2 全局物理空间预算

把每个实际承载信道文件的文件系统分别记为 `i`；分块暂存、组装临时文件和最终信道内容都必须纳入其所在文件系统的预算，不能只计 DataDir。

进程启动顺序固定为：

1. 信道状态为内存态，重启后不存在可继续使用的信道；先清扫所有上次遗留的信道分块、组装临时文件和最终临时内容。
2. 所有清扫成功后，对每个承载信道文件的文件系统读取可用字节，记为 `F0_i`。读取失败时信道内容上传 fail closed。
3. 每个文件系统的保护底线 `floor_i = ceil(F0_i × 0.10)`；对应的全局信道预算 `B_i = max(0, F0_i - floor_i)`。10% 是服务端保护底线，不是客户端配额；启动后固定此快照，不随可用空间变化重算。

每次 content init 都必须先在任何请求体字节写入前完成以下原子操作：

- 在同一 ChannelManager 单写者锁内，验证该信道内容槽为 `empty`、占用它并转为 `uploading`；不同 request/idempotency key 的并发 init 中只有一个成功，另一个返回 409。相同 key 返回同一上传会话。
- 根据已固定的逻辑额度和存储流程，逐文件系统计算本会话的物理峰值上界 `R_i`。`R_i` 包含该文件系统上的分块暂存、组装/最终文件在临时副本并存期间的空间，以及 manifest、归档目录、文件系统分配等有界开销；按最坏同时存在的物理字节预留，不以逻辑 part 字节数代替。分块大小、item 上限和归档上界必须使每个 `R_i` 可计算且有限。
- 在同一锁内逐文件系统原子验证 `heldPhysical_i + uploadingReservations_i + R_i <= B_i` 并记录 reservation。每个上传会话的 reservation 计一次完整峰值 `R_i`；实际已写入的暂存字节属于该 reservation，不重复累加。finalize 时将 reservation 原子替换为最终保留文件的实际物理字节；abort、kick、close 和清扫时释放 reservation/实际占用。
- 同时逐文件系统读取当前 free bytes `Fnow_i`。要求每个 `Fnow_i >= floor_i + outstandingUnmaterializedReservations_i + R_i` 后才允许开始传输。普通分享不计入 `B_i`，但其写入仍会降低同一文件系统的 `Fnow_i`，因此会影响新信道 init 的动态检查。普通分享与信道写入在检查后仍可能竞争磁盘；若发生 ENOSPC，只清理未完成上传及其 reservation，保留已有活跃内容。
- `F0_i` 快照预算不因普通分享而变化。之后每次新传输都对每个相关文件系统用当前 `Fnow_i` 检查保护底线；已活跃或已预留的信道不因普通分享占用增加而被自动删除、kick 或缩短寿命。

磁盘比例、预算、reservation 数量和剩余空间不得出现在 UI、公开 API 或用户可查询的管理信息中；超限只返回通用的“暂时无法接收新内容”错误。此保护不向客户端透露机器盘容量。

## 2. 路由与身份凭据

| 操作 | 账号模式 | 无账号模式 | 凭据与边界 |
|---|---|---|---|
| 创建信道 | 登录、upload 权限、CSRF | 无 UploadAuth 时按现有开放策略；启用 UploadAuth 时要求 API key | 设置下述目的绑定 `sender_key` cookie，不在响应体返回密钥 |
| 上传内容 init/chunk/finalize | 账号上传策略 + 信道 `sender_key` | `sender_key` + 已启用时的 UploadAuth | 信道域端点；内部复用现有分享创建服务；不接受 `X-Channel` 作为授权 |
| 加入信道 | 显式公开可匿名；非公开需登录和 download 权限；身份只从会话解析 | 匿名加入 | 已登录会话的写操作校验 CSRF |
| 查看状态 | 按明确 sender/receiver 角色验证 | 同左 | sender-only 只认 `sender_key`；receiver-only 只认 `seat_token` |
| 放行、拒绝 | sender-only | sender-only | `sender_key` + 同源/防 CSRF 校验；请求绑定当前 `seat_id` |
| 踢出、关闭 | sender-only | sender-only | `sender_key` + 同源/防 CSRF 校验 |
| 心跳、内容读取、leave | receiver-only | receiver-only | `seat_token` 专用 header；`claim_token` 专用 header；令牌不进 URL 或浏览器持久存储 |
| 页面 | 信道感知 access middleware；先解析会话，再按 visibility 判定 | 公开页面可匿名 | 不整体豁免 Gate；`/c/:id` 在 `/:slug` 前注册 |

`sender_key` cookie 属性固定如下：

- `HttpOnly`、`SameSite=Strict`，不设置 `Max-Age` 或 `Expires`（会话 cookie）。
- `Path=/api/v1/channels/<完整 channel id>`，精确绑定单条信道，不使用父路径。
- 仅当请求的外部 scheme 已由 TLS 或已配置的可信代理确认是 HTTPS 时设置 `Secure`；不能信任任意客户端提供的转发头。
- 信道进入 terminal closed 状态时服务端主动清除此 cookie；清除使用相同 name、Path、SameSite、Secure、HttpOnly 属性并设置删除语义。刷新页面不清除；浏览器会话结束后不承诺恢复。

## 3. 状态、原子操作与幂等

- 信道 ID 使用 CSPRNG 至少 128 bit；默认一席。发送端预置后，首个获准接收方加入触发发送；双方在线是发送前提。
- 席位轴：`open → pending_admit → paired`；拒绝/同意超时回到 open；leave、kick、撤回、页面关闭释放席位。
- sender 状态为当前接收方提供非秘密 `seat_id`。`admit/reject` 必须携带用户实际看到的 seat ID；服务端在同一锁内校验它仍指向当前 pending 席位。决策幂等键按 channel + seat ID + request ID 绑定；席位过期的终态也要在重放期内固定，旧决策不能作用于后续接收方。
- 内容轴：`empty → uploading → held`。槽位必须在 init 收到任何分块字节前原子占用；上传失败清理临时文件并回到 empty；`pending_admit` 阶段服务端没有内容文件。
- create/join/admit/leave/content init/content finalize/kick/close 均有 request id 或等价 idempotency key。相同 key 重试返回相同会话或决策，不重复 side effect；相反的 admit 决策返回冲突。
- 同信道不同 content-init key 在 `uploading` 或 `held` 状态返回 409。相同 init key 返回相同 upload session；finalize 重试返回同一结果。kick 重试保持已释放的 open 状态；close 重试保持 closed。channel lock 串行化 kick 与 close，close 是终态。
- 信道没有产品 TTL。发送者离开后不接受新加入、不再上传；已有活跃占用继续由接收页和释放规则管理。重启清理所有孤儿信道文件，UI 明示信道不跨进程重启。
- 接收页前台心跳 5 秒、隐藏页 30 秒；超过 90 秒无活证据才按崩溃/断网处理。存活页长暂停不自动释放，发送者可随时 kick。
- 发送端状态读取超时为 90 秒，以容纳后台标签约每分钟一次的定时器节流；后台冻结更久仍可能被标记离线，页面恢复后状态轮询会重新确认。
- 匿名放行窗 2 分钟；过期后释放 pending 席位。发送端离线时需其确认的 join 返回 410。
- 页面关闭/刷新走非 bfcache `pagehide` 的 keepalive/beacon `leave`，立即释放并销毁临时内容。下载管理器随接收页终止，不允许 ticket lease 独立续命。同页网络中断恢复最多 2 分钟；leave、刷新或关闭后旧令牌失效。
- 对相同幂等 key 的重试不重复计入 join rate limit。

## 4. Join 限速口径

- 来源 IP 只通过 Gin 的 `ClientIP()` 与显式可信代理配置解析；实现不得自行读取/信任任意 `X-Forwarded-For`。未列入可信代理的转发头一律视为不可信。
- 首版候选阈值：每 IP 每滚动 60 秒最多 30 次；每信道每滚动 60 秒最多 5 次。
- 一个 join request id 首次通过语法、可见性和身份授权校验时计为一个新 join attempt；成功、`seats_full` 和已存在 pending 的冲突都计数。认证/可见性失败和无效请求不计数；同一 request id 的重试不重复计数。超过限额返回 429 与 Retry-After。

## 5. 内容隔离、交付与恢复

- D1：`POST /api/v1/channels/:id/content` 的 JSON `{manifest}` 创建上传会话；`PUT /api/v1/channels/:id/content/:uploadID/parts/:partKey` 写分块；`POST .../:uploadID/finalize` 完成入库；`DELETE .../:uploadID` 放弃未完成会话。创建、写块、finalize 和 abort 每次都校验信道 `sender_key`，无账号模式另逐请求校验 UploadAuth API key；账号模式逐请求校验 upload 权限与 CSRF。内部复用现有 `SharesHandler`/`PasteService`，不把 `X-Channel` 当授权。
- D2：每 part 按 4 MiB 固定块写入，最后一块为该 part 的剩余字节；每块使用十进制 `Upload-Offset` 与十六进制 SHA-256 `X-Chunk-SHA256`。manifest 必须声明所有 part 的逻辑字节总数；init 时固定完整 `effectiveUploadLimit` 快照并检查总数，此后每块只能写入声明范围。相同 offset/长度/hash 重试返回已接受，不重复追加；同 offset 不同内容、越序/未对齐 offset 或超出声明均冲突。
- init 在 `empty → uploading` 原子占槽并原子预留全局物理峰值；单写者锁同时串行化同信道 slot 与所有信道 reservation。令 `L` 为 init 时冻结的有效逻辑额度、`N` 为 manifest part 数（`1 ≤ N ≤ 96`）、`M=64 KiB` 为 manifest 上限、`Z=1024×(N+1)` 为 ZIP/目录有界开销、`Q` 为该文件系统的 allocation block 大小。首版在本地文件存储上的 reservation 为 `R=3L+2(M+Z)+(N+3)Q`；远端 S3 存储的本地 staging reservation 为 `R=2L+M+Z+(N+1)Q`。它覆盖 part 暂存、ZIP/manifest、文件系统块取整及最终副本同时存在的峰值。预算以启动清扫后的每文件系统 `F0`、10% 保护底线和动态 `Fnow` 检查为准。普通分享不计入信道预算，但会通过 `Fnow` 降低新 init 的可用空间。预留失败 fail closed；ENOSPC 只 abort 未完成上传，保留 held 内容。
- 临时 paste 标记为 ephemeral/channel，内部保留 168 小时孤儿兜底时间；它不进入 `expires_at` 用户响应，也不是产品 TTL。存储读取不得仅因兜底时刻删除活跃信道内容；周期清扫只有在 channel manager 确认内容不活跃后才删。进程启动先取得共享存储范围内的原子单写者锁，再清理可识别的未完成 finalize、信道 staging 与 ephemeral paste，最后采集 `F0`。finalize 在内容和 metadata 提交期间保留可识别的 pending 对象；清扫顺序必须能覆盖“内容已写、metadata 尚未写”崩溃状态。pending slug 计入普通 slug 占用；清扫仅在 metadata 确认其为 ephemeral channel paste 时删除最终 slug 内容，没有 metadata 或为普通 paste 时只清理 pending 对象。
- 信道 manager、内容席位和全局磁盘 reservation 是进程内单写者状态；P1a 信道仅支持单进程服务。文件系统使用共享 DataDir 独占锁，S3 使用共享 prefix 条件写锁。后台 janitor 仅在锁持有并完成初始化时启动；每轮清扫持有 storage writer guard 完成所有权校验和删除操作，关停等待在途清扫结束后才能释放锁。运行中的服务不得外部删除或替换 writer lock。无法取得或不支持锁时，所有信道路由 fail closed，且不得运行全局启动清扫或后台 janitor。进程崩溃遗留的锁也 fail closed；仅在确认无信道写入进程后才可人工移除。Lambda/serverless 与多实例共享存储部署不得绕过此门禁。
- Leave/close 后如存储删除失败，按 channel 保留 pending release 并阻止该信道重新装填内容；后续 leave/close 和后台清理可重试删除，不得覆盖旧 slug 或提前释放 held 预算。
- Paste 创建在写入 content 与 metadata 的整个区间持有共享存储范围内的 per-slug 原子 reservation；普通创建与 channel finalize 使用同一 reservation，解决 Exists 检查后的并发写入。进程崩溃遗留的 reservation 会让该 slug 保持不可用；只有确认没有该 slug 的活跃创建后才可人工移除。
- 普通分享 metadata/content/receive、legacy slug、raw/json、meta 路径对 ephemeral 记录统一 404；管理台账不列临时内容；普通 publicShare 与 Gate 不放行临时记录。
- 同一内容版本的 multipart boundary 稳定；响应提供 ETag、Accept-Ranges，支持 If-Range/Content-Range；Range 按完整 multipart 响应字节寻址。
- 内容只有在 receiver seat 与有效 claim 绑定时可读。发送者撤回、kick、close 必须取消在途写出。成功写完后销毁；leave/关闭/掉线释放流程销毁临时内容。单读由独占 claim/seat 锁保证，不把“删除先于交付”写成永久不变量。
- 活跃内容不因磁盘预算、清扫或普通分享写入自动删除。完成、显式释放、掉线后的释放倒计时及重启孤儿清扫按既定信道生命周期删除。普通分享 TTL/访问行为保持现状。

## 6. 身份与范围

- 账号模式中，join 可从会话解析身份的接收方显示用户名并零确认，包括显式公开信道；必须测试非预期登录账号通过公开链接自动接收的风险。
- 账号模式匿名接收方进入 pending_admit；无账号模式每次立即发送都由发送者确认。确认前服务端没有内容文件；确认显示匿名身份及加入信息。
- 信道临时身份只在信道存续期间对发送者可见，不写长期回执。接收者在 join 前看到身份披露。
- 默认一席；不做 P1b 份数、P1c 回执、P1d 管理图表、P2 渐进交付、P2P、owner/ACL/分组或跨页面恢复。

## 7. 分批门禁

- 本文件为 P1a-0；审阅通过后才进入 G1。
- G1–G6 分批独立提交、测试、回传 SHA/文件/命令与输出/浏览器证据/边界/下一批计划；需修改则修复后重送审，通过前不得推进。
- 浏览器测试仅使用隔离数据和端口；G5 使用真实双设备/双浏览器。
- 最终审阅覆盖上传和 Range 恢复、访问控制、临时内容防旁路、pagehide/leave、异常掉线、kick/撤回中断、代理配置及部署演练。最终审阅通过前不部署；完成线上验收后才算 Done。

## G1 addendum：创建资源边界与接收页角色

- `/c/:id` 是接收方页面。私有信道页面要求登录和 `download`；sender 不能用 `sender_key` 打开 `/c/:id`，因为该 cookie 的 Path 只匹配 `/api/v1/channels/<id>`。sender 控制台使用站点主页面和信道 API。
- G1 尚无 sender heartbeat/reaper 时，Manager 最多保留 1024 条 live channel；live channel 与 closed-create key tombstone 合计最多 61,440 条，tombstone 保留 60 秒。每来源 IP 每滚动 60 秒最多创建 30 条新信道，limiter 最多跟踪 2048 个 IP。幂等重试不重复计数；超限返回 429，达到记录上限返回 503，不自动删除仍存活的信道。
- G1 的 `DELETE /api/v1/channels/:id` 只关闭空信道：要求该信道的 `sender_key`、同源校验和幂等键；create admission 与 close 共用串行锁。成功后删除信道及 create replay 结果、写入短期 closed-key tombstone 并清除 cookie。关闭重试返回 204；同一 create key 在 tombstone 有效期内返回 409，过期后才作为新建请求。
- Create 限额只在新 channel 成功提交后计数；同 key 并发重试只计一次，创建失败不消耗额度。Manager 将 live channel 数与 closed-key tombstone 数合计限制在 `MaxChannelRecords` 内存边界内。
- G2/G6 按 D6 增加仅清扫无内容、无占用且 sender 已离线/信道已终态的规则；不为腾位置驱逐活跃信道。

## G5b：浏览器下载 ticket 与表示锁

- `POST /api/v1/channels/:id/content/ticket` 使用 `seat_token` 与 `claim_token` header 签发当前 seat 的 browser-download ticket；同源校验与私有信道账号/download 权限逐请求复用内容下载规则。公开信道允许匿名，但仍须有效 seat/claim。每 seat 只保留一个有效 ticket；重签撤销旧 ticket。ticket 为 256-bit 随机 URL path capability；Manager 只保存 ticket hash 与 channel/seat generation 绑定，不持久化票据明文。
- `GET /api/v1/channels/:id/content/download/:ticket` 只接受 ticket capability，不从 `sender_key`、普通 seat/claim header 或 `X-Channel` 降级授权。私有信道仍在每个 GET 校验登录与 download 权限；公开信道允许匿名，但 ticket 必须绑定当前 seat generation。响应设 `Cache-Control: no-store` 与 `Referrer-Policy: no-referrer`。访问日志使用路由模板，不记录实际 ticket 路径。
- native attachment 输出为单文件原始内容，或现有 `zip-v1` ZIP；不得把 app 使用的 `multipart/form-data` 原样保存成下载文件。App 路径的 Range 字节空间仍是完整 multipart 响应；browser-attachment 路径以原始文件/ZIP 的实际字节为准。
- Seat 的 delivery representation 初始为 unset；只有首个 GET 成功取得独占 `DeliveryLease` 时，Manager 才在同一锁内将它原子锁定为 `app-multipart` 或 `browser-attachment`。签票不锁模式。鉴权失败、内容未就绪、已有 lease 或模式冲突均不得改变模式/offset。首个 GET 一旦取得 lease，即使零字节失败也保留模式；重试、重签和暂态恢复不得清 offset 或改变同一内容版本的 ETag。跨表示请求返回 409 `delivery_mode_conflict`。
- 每个表示分别遵守自身固定 total/ETag/Range 字节空间；Range 起点不得跳过尚未写出的前缀，delivery high-water 只按连续成功写出的字节推进。相同 seat 上 app GET 与 ticket GET 竞争同一个 `seat.delivery`，不允许并发写出。被取消的旧 lease 迟到 `FinishDelivery(complete=true)` 不得结算成功、删除当前内容、撤销新票或清除后续 lease。
- 暂态 `released` 是 90 秒失活后的两分钟恢复窗口：保留 ticket、generation、representation 与 offset。ticket GET 返回 409 `delivery_not_ready`，不恢复 seat、不刷新活证据；同一接收页的 status/heartbeat 恢复到 paired 后，同一 URL 可按原表示继续。票据签发、验证、轮换都不刷新活证据；成功写出的字节沿用既有活证据规则。
- 轮换 ticket 与 GET/lease 获取共享 Manager 串行化边界。若旧 ticket 正持有 native lease，先撤销旧票并 Cancel 在途 lease，但保留 mode/offset；旧 lease unwind 前，新 GET 返回 409 `delivery_busy`。若在途 lease 为 app-multipart，签票返回模式冲突且不得取消 app lease。leave/kick/close、交付完成与最终 seat 释放均撤票并取消在途响应；页面关闭/刷新通过非 bfcache pagehide 的 leave 立即触发此行为。ticket 不设置独立 heartbeat/lease。
- `released` 暂态票据仍须通过当前 seat/claim/released 窗口与账号权限验证；实际已送达 `leave` 后旧票即失效。Pagehide 未送达时仍只走既定 90 秒活证据与两分钟释放兜底。公开票据是 bearer capability；P1a 不承诺通过 URL 识别具体浏览器标签页。
- 撤票、取消响应、临时副本删除与 held 预算是不同结果。source/prepared 临时副本的预算仅在实际清理完成后释放；最终内容 Delete 失败时必须保留 pending release、held 预算并阻止重新装填，不能把 Cancel 当作 Delete 成功。
- native GET 路由必须加入 `canonicalErrors` 流式旁路，不得缓冲完整响应。浏览器路径在接收页首次 GET 前选定；选定后不得跨表示切换。UI 明示“已交给浏览器，保持本页打开”；pagehide/leave 撤票并中断下载。桌面/移动下载、慢速 Range、轮换中断、pagehide 与反代 URI/referrer 日志作为 G5/G6 验收项。
