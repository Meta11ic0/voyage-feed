# VoyageFeed（航路行情）架构设计书 v0.1

状态为待实现。本文件可直接作为实施输入。第 1–8 节说明架构和范围，第 9–13 节规定实现默认值、交付和验收。执行时按第 13 节顺序完成，允许在不改变接口与验收结果的前提下决定内部代码结构。

## 1. 系统目标

运行一个以《海贼王》角色为标的的模拟证券市场。服务启动、载入已发布的标的池后，按自己的时钟持续生成逐笔成交。客户端可以查标的、最新价、板块和历史 K 线，也可以订阅实时行情。即使没有客户端在线，市场也照常运行。

行情系统是一个可独立启动的项目。后续程序化交易后端可以像普通客户端一样订阅行情和拉取历史。v0.1 覆盖模拟行情的生产、保存、查询、推送和重启恢复。委托、订单簿、撮合、账户、结算和策略执行属于后续范围。

| 已明确的要求 | v0.1 的设计选择 |
| --- | --- |
| 服务启动后持续产生行情 | 单个常驻服务，默认每秒推进一秒模拟时间，全天运行 |
| 首批使用《海贼王》角色 | 全量目标为某个漫画章节版本下的具名正史角色，开发可用小样本启动 |
| 初始总市值参考出场次数，初始股价参考人气 | 出场次数采用对数权重，价格使用有重叠的人气分档，避免人气差距直接放大为价格差距 |
| 运行后的涨跌完全随机 | 每个标的每一步独立抽样，人物资料和阵营仅用于初始化及展示 |
| 一个进程持续更新行情 | Go 服务内运行时钟、行情引擎、REST 和 WebSocket，PostgreSQL 保存持久状态 |

## 2. 真实行情链路与本项目的部署

### 2.1 交易所行情与下游行情服务

交易所接收新增委托、撤单和改单，由撮合系统维护订单簿并生成成交。交易所的行情发布链路把这些变化发成带序号的事件，供下游订阅。NYSE Pillar 集成行情同时提供订单簿变化、逐笔成交、集合竞价不平衡和停牌等状态。最新成交价来自已经发生的交易，买一、卖一和各档挂单量来自当时的订单簿。[NYSE Pillar 行情规范](https://beta.nyse.com/publicdocs/nyse/data/NYSE_Pillar_Integrated_Feed_Client_Specification.pdf)

券商、行情商或自建行情后台接入上游事件，按序更新本地状态，把成交或报价推给策略和前端，也可以保存历史、聚合 K 线。网络断开后，需要用序号发现缺口并补齐，或者用标明序号的快照重建状态，再接上实时更新。Nasdaq 的 ITCH 增量行情与 GLIMPSE 订单簿快照展示了这套恢复方式。面向应用的接口可以用 WebSocket 分别提供成交、报价、K 线和修订消息，Alpaca 的股票行情接口是一个例子。[Nasdaq ITCH](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHSpecification_5.0.pdf) · [Nasdaq GLIMPSE 恢复说明](https://nasdaqtrader.com/content/technicalSupport/FAQs/Nasdaq_Glimpse_FAQ.pdf) · [Alpaca 实时股票行情](https://docs.alpaca.markets/us/docs/real-time-stock-pricing-data)

| 环节 | 本项目 v0.1 |
| --- | --- |
| 上游事件 | 由随机引擎持续给出模拟成交和跨日事件。成交带市场 ID、标的代码、时钟和序号 |
| 行情处理 | 从同一批成交更新最新价、日内统计和 K 线，并保留原始成交供查询和回放 |
| 分发与恢复 | 用 REST 取快照和历史，用 WebSocket 接续实时增量。缺口按序号从事件表补齐 |
| 买卖报价与深度 | 首版行情范围为逐笔成交及其派生数据。若后续要按买卖价差、挂单量或部分成交评估策略，再设计模拟订单簿及成交模型 |

### 2.2 单进程部署

~~~mermaid
flowchart LR
    subgraph Service["行情服务（一个进程）"]
        Clock["市场时钟"] --> Engine["行情引擎"]
        Engine -->|提交后唤醒| Broadcast["实时分发"]
        REST["REST 查询"]
        Broadcast --> WS["WebSocket"]
    end
    Engine -->|同一事务写入| DB[("PostgreSQL")]
    DB -->|按序读取已提交事件| Broadcast
    DB -->|快照与历史| REST
    REST --> Client["行情客户端 / 交易后端"]
    WS --> Client
~~~

- **行情引擎**按逻辑时钟生成一批事件，在一次 PostgreSQL 事务中保存成交、更新最新价和 K 线、推进时钟。用同一份成交数据生成所有行情视图。
- **实时分发**在同一进程里运行。引擎提交后发一个内存通知，分发循环按序读取数据库新事件并推送给在线连接。通知可以合并，分发循环也定期检查数据库，因此漏一次通知仍可补发。
- **REST**直接读取数据库。进程内缓冲只负责加快推送，快照、历史和恢复位置都由数据库提供。
- **本地部署**用 Compose 启动行情服务和 PostgreSQL 两个容器，数据库使用持久卷。角色数据导入是独立的一次性命令。可先导入开发样本并启动，之后导入全量标的版本。

v0.1 只部署行情进程和数据库。进程里可以用不同的 goroutine 分别运行行情时钟和连接分发。每个连接使用有界发送队列，发送慢的客户端不会拖住生成器。主要写入按秒批处理，K 线更新与成交同事务提交，在线客户端共享一个分发循环。需要多台网关共同分发时，可以考虑 Redis Streams 等事件总线。K 线聚合拖慢成交写入时，可以把聚合移出主事务，交给独立 worker。是否拆分以写入延迟和分发积压为依据。

## 3. 标的池和初始参数

### 3.1 角色资料

按漫画截至某章冻结标的池，收录该范围内可识别的具名正史角色。同一角色的别名归到一个稳定的角色 ID。同名角色按 ID 区分，交易代码一经发布保持稳定。每只标的设一个用于统计的主阵营，可以再保留多个阵营标签，主阵营未知则归入“其他”。首版用主阵营作为板块，统计时每只标的只归属一个板块。

出场次数按漫画章节去重，正文中确有出场的回忆画面也纳入。仅被提及、仅在封面出现，以及无法确认身份的画面另记。资料缺失保留空值，经核实的零出场记录为零。人气榜保留榜单版本、名次和未上榜状态。角色目录与章节索引可作为候选来源，发布前抽查高频角色、别名和同名角色。全量角色总数以导入校验结果为准。[正史角色目录](https://onepiece.fandom.com/wiki/List_of_Canon_Characters) · [章节索引](https://shimotsukicodex.com/atlas.html) · [官方人气榜](https://onepiecewt100-2026.com/en/final-rankings/)

### 3.2 初始化计算

| 参数 | 规则 |
| --- | --- |
| 目标初始总市值 | 按全市场设定的初始市值预算分配。角色权重为 `1 + ln(1 + 出场章节数)`，缺失计数的角色暂用基础权重并保留缺失标记。总预算是模拟配置 |
| 初始股价 | 用人气榜确定价格区间，区间内根据标的 ID 和初始化种子抽取。示例区间为前 10 名 22–35 元、第 11–30 名 16–30 元、其余上榜角色 10–24 元、未上榜 8–20 元。分档有重叠，具体区间可调 |
| 总股数 | `向下取整(目标初始总市值 ÷ 初始股价)`。发布时校验股数为正，实际初始总市值由整数股数与股价相乘 |
| 流通比例 | 用阵营内的角色身份近似持股集中度。建议阵营负责人 30%、核心成员与身份未知 50%、普通成员 70%。人工覆盖可记录理由，比例属于待定默认值 |
| 流通股数 | `向下取整(总股数 × 流通比例)`。发布时校验大于零且不超过总股数 |

例如目标总市值 2.5 亿元、初始价 25 元、流通比例 50% 时，总股数为 1,000 万股，流通股数为 500 万股，初始流通市值为 1.25 亿元。数据库用整数表示分、股和成交额。价格精确到分，读写时验证乘积不会超出存储范围。

初始化数据按版本发布。已启动的市场固定标的版本、随机模型版本和随机种子。换数据或参数时创建新的市场实例，独立保存历史。持续运行用的起始价来自这次导入，之后每一步用上一次有效价格计算。

## 4. 时钟和随机行情

### 4.1 时钟

市场每秒尝试推进一个逻辑步，从已提交的 `logical_tick` 恢复。第一版按一天 24 小时运行，以北京时间切分模拟交易日。跨日时把上一日末的有效价格带作新一日的昨收，重置日内成交量、成交额与 OHLC。新市场首日用初始参考价作为昨收。跨日状态、当步成交和时钟位置一起提交。后续可增加定时开闭市的配置。

只有当前步提交成功才进入下一步。数据库写慢时，模拟时钟随进程放慢。停机恢复时接着生成下一逻辑步，停机期间市场时间暂停。行情同时保存模拟事件时间和服务器生成时间，API 状态暴露两类时间，客户端据此区分虚拟日历和真实停机。

### 4.2 每步生成

1. 按稳定的标的 ID 顺序遍历标的。每只标的独立抽取是否成交，示例配置为每秒 2% 的概率。这会产生标的间自然的疏密差异，首版对所有标的使用同一概率。
2. 对将成交的标的，从对称区间均匀抽取整数基点变化，示例为 `-20…+20 bps`。按 `上一有效价格 × (1 + 变动基点 ÷ 10,000)` 计算候选价，再按最小价位四舍五入。候选价超出第 10.3 节的正价范围时，这一步不生成该标的成交。舍入后价格没变时，仍可生成一笔平价成交。
3. 成交数量独立抽样，示例为随机 1–10 手，每手 100 股。逐笔事件记录成交价格、股数和模拟时间。没有成交的标的继续沿用有效价格，成交量和成交额保持原值。
4. 每次抽样由 `市场种子 + 模型版本 + 标的 ID + 逻辑步 + 抽样用途` 确定。三个用途分别为成交、变价与数量。固定版本和种子可以重现行情，同一逻辑步因故障重做时抽样一致。

初始参考价决定随机变化的起点。人物人气、出场次数、身份和阵营都不参与涨跌或成交抽样。随机变动的候选基点正负对称，实际观察到的价格还受最小价位、舍入和平价成交影响，因此有限样本里的涨跌次数可以不相等。板块涨跌来自成员价格的汇总。

## 5. 行情视图和数据表

| 数据表 | 保存内容 |
| --- | --- |
| `market_instances` | 标的版本、规则版本、种子、时钟起点、最近提交的逻辑步、最新事件序号 |
| `instruments` | 市场 ID、角色 ID、交易代码、名称、主阵营、初始价、总股数、流通股数、来源版本 |
| `instrument_states` | 每只标的的最新成交价和事件序号、昨收、本交易日 OHLC、成交量与成交额 |
| `market_events` | 按市场与事件序号保存逐笔成交和跨日事件，按标的与时间索引。事件序号由市场实例的已提交高水位连续分配 |
| `bars_1m` | 按市场、标的、模拟分钟保存 OHLC、成交量、成交额和最后成交序号 |

每个逻辑步一次事务。成交写入事件表，最新快照和当分钟 K 线按事件生成顺序更新，最后持久化当前逻辑步和最高事件序号。无成交的逻辑步也保存时钟位置。提交后，客户端才可能看到这一批数据。[PostgreSQL 事务](https://www.postgresql.org/docs/current/tutorial-transactions.html)

- **快照**提供参考价、最新成交价、最近成交时间、昨收、日内范围、成交量和成交额。首次成交前最新成交价为空，展示参考价。
- **K 线**用同一时段的第一笔、最高一笔、最低一笔和最后一笔得到 OHLC，数量及成交额求和。无成交的分钟默认留空。1 分钟线落库，5 分钟和日线由同一交易日内的分钟线聚合。用快照中的已提交逻辑时间判断当前 K 线是否收盘。
- **板块和全市场**按当前有效价格与流通股数计算流通市值。板块涨跌幅为 `当前流通市值之和 ÷ 昨收流通市值之和 − 1`，同时返回上涨、下跌、平价和未成交标的数。读取结果附带适用的事件序号。

## 6. 查询和实时订阅

REST 路径以 `/v1/markets/{market_id}` 为前缀。时间按模拟事件时间查询，范围采用左闭右开。

| 路径 | 用途 |
| --- | --- |
| `GET /status` | 市场 ID、运行状态、模拟日、已提交逻辑时钟及事件序号、最早可查询事件序号 |
| `GET /instruments` | 角色、交易代码、板块和发行参数，支持分页 |
| `GET /snapshot` | 一致性读取最新行情与 `as_of_seq`，作为前端初始化基线 |
| `GET /events?after_seq=&until_seq=&limit=` | 逐笔成交及跨日事件，用于补数据和历史回放。首次读取返回固定上限，后续分页沿用 |
| `GET /bars?symbol=&interval=&from=&to=` | 分钟线及聚合周期。返回周期起止和当前 K 线状态 |
| `GET /sectors` | 每个阵营的流通市值、涨跌幅和涨跌家数 |
| `WS /v1/stream` | 按市场订阅全市场事件，支持从已有事件序号续订 |

一笔实时成交消息示例，金额以分、数量以股计。

~~~json
{
  "schema_version": 1,
  "market_id": "one-piece-v1",
  "event_seq": "1842",
  "type": "trade",
  "symbol": "OP000123",
  "logical_tick": "608",
  "market_date": "2026-11-17",
  "event_time": "2026-11-17T01:10:08Z",
  "generated_at": "2026-11-17T01:10:09Z",
  "price_cents": "2518",
  "quantity": "300",
  "turnover_cents": "755400"
}
~~~

1. 客户端先请求快照并保存 `as_of_seq`，再携带市场 ID 和该序号订阅。服务先把连接挂到实时分发并开始缓冲，然后在同一数据库读快照内取得当前事件上限、按序读齐它与客户端序号之间的尾段。例如快照到 1200，建连时数据库已到 1212，先重放 1201–1212，再接上缓冲中 1213 及之后的事件。尾段超过单连接缓冲可承受的范围时，客户端先走 REST 分页追平。
2. 客户端按 `market_id + event_seq` 去重，并检查全市场流中的连续性。短暂断线从最后一个连续序号恢复。缺口较大时通过 REST 分页追平，再恢复订阅。
3. 每个连接都有队列上限，慢连接超限后由服务断开，由客户端续订。历史超出保留期时，返回 `cursor_expired`，客户端重新拉快照。实时心跳附带服务器时间与已提交进度，长时间没有市场成交也能维持连接。

## 7. 并发、恢复和运行容量

### 7.1 故障恢复

- **防止重复生成**。引擎用同一条专用数据库连接持有市场的 PostgreSQL 会话级互斥锁并提交行情写入。连接失效即停止推进。每次写入检查数据库中的逻辑步和本次预期的前一步一致。启动第二个实例时，拿不到锁就不接管行情。
- **提交前故障**。未提交的一步由数据库回滚。进程按最近已提交位置恢复，使用原种子重算下一步。
- **提交后故障**。已提交成交、快照和 K 线保留。连接重新订阅时从数据库补齐，即使进程在提交后、推送前退出，也能取回对应事件。
- **数据库不可用**。暂停时钟，状态和就绪探针报告不可用。恢复连接、取得写入锁并确认数据库位置后续跑。备份保留市场配置、原始事件、派生行情和逻辑时钟。恢复早期备份需发布新的市场 ID，避免旧客户端把重用的事件序号当成已读数据。

### 7.2 容量和监控

以 1,500 只标的、每只每秒 2% 的成交概率估算，平均 30 笔/秒，连续一天约 259.2 万笔。无成交时，数据库仍保存市场时钟。1,500 只用于估算负载，实际标的池以导入为准。

逐笔保留期可先配置为 7 个模拟自然日，K 线按更长窗口保留。逐笔到期清理在同一服务的低频维护循环里分批执行，优先压测批量写入、历史范围查询和并发订阅，再根据测得的空间占用、清理成本和数据库延迟决定是否分区。

监控时钟最后推进时间、单步写入耗时、数据库失败与重试、已提交事件与已广播事件之间的差值、连接队列超限数，以及原始事件占用空间。优雅退出时先停接新订阅，结束或回滚当前事务，再断开连接。

## 8. 启动和验收

首次部署按数据库迁移、发布标的池、启动服务的顺序进行。服务启动时检查数据库结构及市场配置，读取已有进度，拿到写入锁后自动开跑。更换标的版本、种子或涨跌规则时用新市场实例。监听地址、时钟倍率等运行设置可在重启时调整。开发样本可以使用更短的真实等待时间和较高的成交概率，以便观察跨分钟和重启恢复。

| 场景 | 通过条件 |
| --- | --- |
| 无人订阅 | 启动后时钟自动推进，成交和 K 线持续保存。连续重启使用相同市场状态 |
| 初始化 | 同版本重复导入得到同一批代码和初始参数。角色别名去重，未上榜和缺失计数标记可查，流通股数处于合法范围 |
| 随机性 | 相同配置与种子从相同初始状态重放得到相同序列。固定标的 ID、种子和初始价后修改人气、出场数或阵营，运行期变价抽样保持一致 |
| 行情字段正确 | 用逐笔记录重新计算快照、K 线、板块涨跌及日内累计值，结果与接口一致。验证平价成交、空分钟和虚拟日切换 |
| 进程与数据库故障 | 分别在提交前和提交后中止进程，再恢复。已提交事件序号连续，K 线累计量准确，第二个引擎不会同时生成 |
| 查询与订阅 | 在快照和订阅之间持续成交，客户端补齐历史与推流之间的缺口。覆盖慢连接、断线、重复消息和保留期边界 |
| 全量容量 | 用全量规模样本压测持续写入、断线补发和订阅并发，记录吞吐、延迟、空间占用以及运行中是否持续积压 |

完成上述功能后，用开发样本执行一次端到端验收。启动后先保持一段时间无人订阅，确认历史已有成交，再接入快照和实时流。停止服务并用同一数据库重启，确认行情从停机前的序号接续。最后导入正式标的池做全量压测。


## 9. 实施工作说明

本节到第 13 节是已选定的实现基线。开始实现时按默认值执行，数字可由配置覆盖。配置覆盖只用于开发、测试或创建新的市场实例。空字段、上游失败和压测失败分别按文中规则处理。验收以实际命令和观察到的结果为准。本设计针对本机运行及开发环境，服务默认只绑定宿主机回环地址。

- 实现位于本仓库根目录 `/home/john/work/voyage-feed`，主程序目录为 `cmd/market-simulator/`。交付 `README.md`、`go.mod`、`go.sum`、`Dockerfile`、`compose.yaml`、数据库迁移、可复现的导入器、内置开发数据、服务和验收入口。
- 使用 Go 1.25、PostgreSQL 17、Go 标准库 `net/http`、`log/slog`、`crypto/sha256`，外部依赖使用 `github.com/jackc/pgx/v5` 和 `github.com/coder/websocket`。解析来源 HTML 用 `golang.org/x/net/html`，文字匹配用 `golang.org/x/text` 的规范化和大小写折叠。选兼容的稳定版本并提交锁定的依赖及容器镜像标签。服务同一进程内划分 `dataset`、`engine`、`store`、`httpapi` 和 `stream` 模块。[pgx](https://github.com/jackc/pgx) · [Go WebSocket 库](https://github.com/coder/websocket)
- 一个可执行文件 `market-simulator` 提供 `serve`、`dataset`、`verify` 三个入口。`serve` 负责迁移、导入首次启动的市场配置、运行引擎及提供 API。`dataset fetch` 抓取公开来源，`dataset build` 离线生成发布文件，`verify` 作为 HTTP 和 WebSocket 客户端执行验收。只在构建标的数据时访问上游网站。运行及自动测试均可使用本地发布文件。
- Compose 包含 `db` 与 `market`，数据库用命名卷，数据库端口不在宿主机发布。PostgreSQL 用 `pg_isready` 作健康检查，行情用 `verify --profile ready`。行情镜像用普通用户运行，发布 `127.0.0.1:8787:8787`。开发数据随镜像发布，全量文件通过独立的 `compose.full.yaml` 只读挂载。启动失败时日志应明确指出出错的变量或文件。README 给出启动、暂停、重启、查看日志、换市场及备份恢复命令。
- 开发数据含 12 个标的与至少 3 个阵营，使用明确标注的测试出场次数和排名，至少有一条排名缺失、一条出场次数缺失、一条明确零出场。全量数据走第 10 节。开发数据的默认成交概率提高到 20%，方便启动后观察。验收另外生成固定的合成压力数据，市场 ID 和数据文件标明用处。

| 配置 | 默认值与校验 |
| --- | --- |
| `DATABASE_URL` | 必填。Compose 为内网数据库连接，首次运行从环境变量或 Compose 开发默认值取，日志屏蔽密码 |
| `HTTP_ADDR` | `:8787`，以 Compose 的宿主机地址控制本机暴露范围 |
| `MARKET_ID` | 默认 `one-piece-dev`，小写字母、数字和连字符，长度 1–48。全量使用新的 ID |
| `DATASET_PATH` | 默认镜像中的 `/app/data/dev.json`，须能读取并通过数据校验 |
| `MARKET_SEED` | 默认 `7`，无符号 64 位整数，协议保留十进制字符串表示 |
| `MARKET_START_TIME` | 默认首次初始化的当前 UTC 秒，支持显式 RFC3339。已有市场沿用已保存的起点，显式传入冲突起点时报配置错误 |
| `TICK_INTERVAL_MS` | `1000`，范围 10–60000。每步始终推进一秒模拟时间。运行慢时延后下步，禁止追赶真实时间 |
| `TRADE_PROBABILITY_BPS` | 默认 200，开发 Compose 为 2000。整数 1–10000，表示每只标的每步成交的万分概率 |
| `MAX_MOVE_BPS` | `20`，整数 1–1000。每笔在对称区间内抽取变动基点 |
| `INITIAL_CAP_BUDGET_CENTS` | `100000000000`，整数 1–10000000000000。预算过小导致任一标的股数或流通股数为零时拒绝初始化 |
| `EVENT_RETENTION_DAYS` | `7`，整数 1–30。按模拟时间滚动计算，K 线首版长期保留 |
| `MAX_STREAM_CLIENTS` | `64`，整数 1–512。超过连接上限时返回 503 |

启动前通过独立迁移锁串行应用 SQL，每个迁移文件在一笔事务中执行并登记版本。应用运行连接及迁移用户使用参数化 SQL。已有市场的标的文件 SHA-256、种子、模型版本、成交概率、变动幅度、初始预算必须与库中保存值一致，冲突时启动失败并打印创建新市场的做法。现有实例仅允许修改监听、步间隔、保留期和客户端上限。多个市场可共享数据库，但每个进程只服务其配置的市场。

## 10. 标的数据构建和发布契约

### 10.1 发布格式和校验

`dataset build --characters <file> --appearances <file> --rankings <file> --source-manifest <file> --cutoff-chapter <n> --overrides <file> --out <file>` 为离线入口。构建输出 UTF-8 JSON，字段包括 `schema_version: 1`、`dataset_id`、`cutoff_chapter`、`sources` 和 `instruments`。`sources` 逐项记录来源网址、获取时间、SHA-256、计数或匹配口径。`instruments` 每行字段固定为 `instrument_id`、`symbol`、`name`、`sector`、`role`、`chapter_count`、`popularity_rank`。数值字段缺失使用 JSON `null`。阵营缺失填 `其他`，角色身份缺失填 `unknown`。身份取 `leader`、`core`、`member`、`unknown`。初始流通比例依次取 30%、50%、70%、50%。字段字符串在去除首尾空白后不得为空。

导入器输出按 `instrument_id` 字典顺序排列。同一批原始文件、截止章节、来源清单及覆盖映射应得到逐字节相同的发布文件。来源的抓取时间从输入清单读取。`dataset_id` 取 `one-piece-c<章节>-<指纹>`，指纹是三份原件与覆盖文件的 SHA-256 十六进制值按输入顺序换行拼接后再次哈希的前 12 位。发布文件和覆盖映射一起保存。输出与相邻可读报告需给出总标的数、覆盖排名和出场记录的数量、正史范围、冲突和未匹配候选。报告可同时带原始姓名及原因。用于手工补录的覆盖文件支持对同一角色录入别名、主阵营、身份和稳定 ID，补录要有来源字段。

首次发布可由正史目录的角色键生成稳定 ID，交易代码用该 ID 的 SHA-256 前 12 位十六进制大写字符加 `OP`。发生截断碰撞时，在覆盖文件登记其中一条的固定代码并重建。新快照保存旧 ID 对照。更名沿用旧 ID；同名但不同身份的角色采用各自的来源键。数据条目重复、代码重复、非法数值、版本不支持、整份来源缺失、章节范围超界，均以非零退出并给出可定位的错误。单条缺失资料仍发布标的，使用初始化的缺失字段规则。

### 10.2 来源处理规则

首次正式数据冻结到漫画第 1198 章。2026-11-18 设计时只读确认了角色目录内嵌 JSON、逐章 CSV 和官方排名 HTML 的数据结构。实例化时保存三份源文件的指纹及抓取时间，覆盖文件使用空对象即可跑第一次构建。来源更新后，按同一截止章节重建并比较报告。

`dataset fetch --out-dir data/raw` 按下表声明的网址保存 `characters.html`、`appearances.csv`、`rankings.html` 及 `sources.json`。下载单文件最多等待 30 秒，失败重试两次。三份来源都抓取成功、具有预期数据结构后才发布目录，已有快照用新目录保存。已存在的快照不覆盖。也可把同一格式的文件直接交给离线构建。上游网络不可用时按本节末尾的降级规则处理。

| 输入 | 读取规则 |
| --- | --- |
| 角色目录 HTML | 读取 `id="punk-records-data"` 的 JSON 脚本。使用 `name,name_jp,chr_id,first_appearance,affiliation`。明确在截止章之后登场的角色排除。正文、回忆、封面或剪影记录可用于确认角色入选，计数仍按下方规则。只有动画或游戏记录的角色排除。缺少漫画证据的候选进入报告，由覆盖文件凭来源补录。正史范围可回查正史角色目录 |
| 逐章出场 CSV | 按表头 `chapter,name,type` 解析。类型 `full,flashback` 计数，`silhouette,cover` 只作为出现证据。遇到新类型记录为未知并排除计数。列缺失、章节格式错误或文件无法支持截止章节时终止构建 |
| 官方日语人气榜 HTML | 只取全球结果面板 `data-fr-static-panel` 且 `data-region="all"` 下的条目。每项取 `data-rank` 与 `data-portrait-name` 的文本，用规范化后的 `name_jp` 或手工别名匹配。同票采用网站给出的并列名次。条目存在但匹配不了时进入报告 |

[角色目录 HTML](https://shimotsukicodex.com/characters.html) · [逐章出场 CSV](https://shimotsukicodex.com/appearances.csv) · [日语全球人气榜](https://onepiecewt100-2026.com/ja/final-rankings/) · [正史角色目录](https://onepiece.fandom.com/wiki/List_of_Canon_Characters)

将角色名录作为标的候选，章节索引提供带类型的单章出场，全球人气榜提供名次。先用角色键或覆盖映射匹配，再用规范化姓名做唯一精确匹配。规范化按 Unicode NFKC、大小写折叠、去首尾空白和压缩中间空白执行；不根据编辑距离猜同一人。角色资料里记录的中文名可优先展示，否则用来源姓名。遇到一对多、重名或别名不一致时，保留候选在报告中。人气未匹配时不编造名次，章节记录无法匹配时不把缺失当零。除非上游给出单章出场类型，无法确认的记录保留为未知。

保留章节编号 `<= cutoff_chapter` 的正文出现和回忆出现，用 `角色 ID + 章节编号` 去重。明确只为剪影、封面和提及时不计数。同一章有可计的正文或回忆记录即保留该章。抽查路飞、索隆、娜美，以及至少两组有别名或重复记录的角色。主阵营优先采用覆盖项。自动归类时按分号拆分 `affiliation`，取第一个非空、无括号状态注释的条目。已知阵营名称由覆盖文件统一，空白归 `其他`。流通身份以覆盖文件为准，未维护者一律用 `unknown`。初次正式发布至少录入能确认的主要阵营负责人和核心成员，剩余角色可以使用缺省值。

`chr_id` 的格式为 `chr:<数字>`，检查全源唯一后使用 `codex:<原 ID>`。缺失或重复时，先合并可确认的重复来源记录，再对不同角色使用 `codex:name:<指纹>`，指纹取规范化日文名、英文名和已知首次漫画章节以换行拼接后 SHA-256 的前 16 个十六进制字符。全部相同仍无法区分的条目进入歧义报告，保留候选直到可从正史来源区分。覆盖文件优先于自动规则。出场数据有可匹配记录、但在截止章前都是剪影或封面时，记录为 0。无可匹配记录时为 null。

原始网页不可访问、网页改版无法解析、全量来源存在缺项时，离线开发样本仍可启动。可完成服务代码并用开发及合成数据验收。全量数据发布项应明确记录哪一份上游材料缺失或哪些角色无法自动核对。完成全量发布至少需要正史候选清单、截取章节及源文件指纹，用户可用同一导入器替换来源快照。提交软件仓库时优先保存发布清单和可再生成流程，源站网页等原始附件依据实际再分发条件处理。

### 10.3 精确的初始化与随机抽样

- 对章节数 `c`，用 `floor((1 + ln(1 + c)) × 1000000 + 0.5)` 得整数权重；缺失使用 `1000000`。分给每只股票的目标市值是 `floor(总预算分值 × 权重 / 权重总和)`，乘积使用 `math/big.Int` 或数据库 `numeric`，然后按第 3 节计算股数。价格区间沿用第 3 节的示例，下界与上界都包含，换算成整数分后抽取。
- 版本名 `random-v1`。按 `模型版本、种子十进制、标的 ID、逻辑步十进制、用途、重试编号十进制` 顺序以换行拼接，采用 UTF-8 和 SHA-256。取摘要前八字节作大端无符号整数，每次重试编号从 0 递增。为在 `[0,n)` 上均匀抽样，拒绝小于 `2^64 mod n` 的值，对接受的值取模；实现用 `uint64(0)-n` 的回绕计算余数。不同用途分别为 `initial_price`、`trade_gate`、`move_bps`、`quantity_lots`。初始价逻辑步固定为 0。
- `trade_gate` 在 `[0,10000)` 抽样，严格小于成交概率时继续。`move_bps` 在 `[0,2M+1)` 抽样后减 `M`。数量用 `1 + rand(10)` 手，每手 100 股。价格使用整数算式 `(上一价分值 × (10000 + 基点) + 5000) / 10000`，正数整数除法取整。候选低于 1 分或高于 `100000000` 分时跳过该笔。价格、数量和单标的日内金额在配置上限内用 `int64`，超范围报错并回滚当步。板块和市场总额用数据库 `numeric` 或应用大整数。
- 市场创建时 `logical_tick=0`、`last_event_seq=0`。首次推进为步 1，模拟时间为 `clock_origin + logical_tick 秒`。每日按 UTC+08:00 切分，开跑当天直接进入连续交易。零成交时只推进时钟。跨日用一条 `session` 事件表示进入新日，其序号早于同一逻辑步的成交。

## 11. 持久化与进程内协作契约

### 11.1 必备列、键与类型

| 表 | 列和约束 |
| --- | --- |
| `market_instances` | `market_id text PK`，`writer_lock_key bigint UNIQUE`（数据库 identity 分配），`dataset_sha256 text`，`dataset_id text`，`model_version text`，`seed numeric(20,0)`，`trade_probability_bps int`，`max_move_bps int`，`cap_budget_cents bigint`，`clock_origin timestamptz`，`logical_tick bigint`，`last_event_seq bigint`，`first_retained_seq bigint`（默认 1），`market_date date`。时钟步与最后序号初始为 0 |
| `instruments` | 主键 `(market_id, instrument_id)`，唯一 `(market_id,symbol)`。保存 `name, sector, role`（text）、`chapter_count, popularity_rank`（可空 integer）、`initial_price_cents, target_cap_cents, shares_total, shares_float`（bigint，均为正）。股数检查 `shares_float <= shares_total` |
| `instrument_states` | 主键及外键 `(market_id,instrument_id)`。`prev_close_cents` 初始化为参考价。`last_price_cents,last_event_seq,last_trade_time,day_open_cents,day_high_cents,day_low_cents` 初始为空。`day_volume,day_turnover_cents` 初始为 0。金额与序号均为 bigint，交易时间为 timestamptz |
| `market_events` | 主键 `(market_id,event_seq)`。保存 `logical_tick bigint, event_time timestamptz, generated_at timestamptz, market_date date, type text`。成交事件另含 `instrument_id text, price_cents bigint, quantity bigint, turnover_cents bigint`，跨日事件四项为空。建立 `(market_id,event_time,event_seq)` 和 `(market_id,instrument_id,event_time,event_seq)` 索引 |
| `bars_1m` | 主键 `(market_id,instrument_id,bucket_start)`，保存 `bucket_start timestamptz`、`open_cents,high_cents,low_cents,close_cents,volume,turnover_cents,trade_count,first_event_seq,last_event_seq`（bigint）。时间按分钟整点对齐。留存逐笔删除后，K 线和标的状态仍保留 |

除表列明确允许空值外，列均为 `NOT NULL`。市场与标的关联使用外键。事件和 K 线的序号用于定位原始数据，保留期内可关联事件表，历史序号不建立阻止逐笔清理的外键。金额整数精度不打折。大额汇总先转 `numeric` 再乘或求和。

### 11.2 写入、发布和维护

1. 新市场发布在一笔事务中写入市场行和全部标的及初始状态。已存在时核对配置和数据指纹。发布幂等，失败整笔回滚。
2. 生成器建立专用连接，调用 `pg_try_advisory_lock(writer_lock_key)`。冲突时退出并报告同市场已经启动。重连时重新抢锁。锁、读取进度及后续写入在同一连接上。迁移用 PostgreSQL 的独立双整数锁键命名空间，避免和市场锁冲突。[PostgreSQL 锁](https://www.postgresql.org/docs/17/explicit-locking.html)
3. 生成事务先锁市场行并读取前一步和最高序号。需要跨日时先保存昨收并重置当日 OHLC、量额，再插一条跨日事件。同一步成交按 `instrument_id` 升序分配连续序号。每个成交插入事件，同事务内更新标的快照和分钟 K 线。分钟 K 线更新时保留开盘，刷新收盘，更新最高最低并累加量、额、成交笔数。最终原子更新市场时钟及最高序号。
4. 未确认提交结果时停止继续写。释放故障连接，恢复独占锁后，以数据库持久化的 tick 和 seq 判定下一步。失败按 250 毫秒、500 毫秒、1 秒、2 秒递增重试，最大 5 秒。数据库不可达时状态接口返回 503，就绪失败，进程继续尝试恢复。恢复后由正常循环自动续跑。
5. 提交成功后向大小为 1 的内存通知通道发非阻塞通知。分发器保持自己的最后读取序号，每次按序最多读取 1000 条并持续追到当前高水位。额外每 250 毫秒检查一次数据库。启动时以当前数据库高水位作为分发起点，历史交给 REST 或 WebSocket 建连补发。每条订阅队列最多容纳 1024 条，满时断开该订阅。分发器送入队列与注册订阅在同一互斥锁下串行，持锁期间不做网络写入或数据库查询。
6. 每推进 60 个逻辑步触发一次保留期维护，维护在运行进程内异步执行，同一市场最多一个清理任务。每次按 seq 从最旧记录起删除不超过 5000 条，连续处理到本轮无过期记录。删除与前移 `first_retained_seq` 同事务提交，水位等于剩余最早序号，无剩余时为 `last_event_seq + 1`。只允许按前缀删除，保留窗口允许短暂延迟，维护失败记录后等下次触发。
7. 优雅退出先停止生成新的逻辑步，最多等待 10 秒完成待提交事务。关闭 WebSocket 和 HTTP 监听，取消后台循环并关闭数据库连接。非正常退出由 PostgreSQL 回滚未提交事务。通过同一逻辑步是否已经提交来判断是否重算。

## 12. HTTP 与 WebSocket 实现契约

本节补充第 6 节。业务路径的市场前缀为 `/v1/markets/{market_id}`。请求其他市场返回 404。状态、快照、板块、历史边界和聚合涉及多次读取时使用只读 `REPEATABLE READ` 事务，响应含其适用的最高序号或事件上界。对外时间统一 UTC RFC3339，交易日单独返回 `YYYY-MM-DD`。交易日和日线边界用 UTC+08:00。事件序号、逻辑步、分值和股数在 JSON 中用十进制字符串。比率、页大小使用 JSON 数字。[PostgreSQL 事务隔离](https://www.postgresql.org/docs/17/transaction-iso.html)

| 端点 | 请求和响应 |
| --- | --- |
| `GET /healthz` | 进程可回包即 200。 `GET /readyz` 仅在迁移、市场初始化及写入锁就绪且引擎正在推进时返回 200。数据库中断或超过 `max(5 秒, 3 × 步间隔)` 未推进时返回 503 |
| `GET /metrics` | 输出 Prometheus 文本。至少包括 `market_tick_commit_seconds` 直方图、`market_event_last_committed`、`market_event_last_dispatched`、`market_stream_disconnects_total`、`market_db_errors_total`。直方图秒桶取 `0.01,0.025,0.05,0.1,0.25,0.5,1,2` |
| `GET /status` | `market_id, dataset_id, model_version, market_date, event_time, server_time, logical_tick, last_event_seq, first_retained_seq, state`。状态为 `running` 或 `paused`。时钟暂停但可读数据库时仍可查询历史 |
| `GET /instruments` | 参数 `after_symbol` 可选，从字典序严格大于该值的位置分页。`limit` 默认 100、最大 1000。返回 `items,next_after_symbol`，最后一页游标为 null。条目使用第 10 节发布字段并附发行数据、参考价和实际初始总市值 |
| `GET /snapshot` | 可选 `symbol` 或 `sector` 做精确过滤，响应 `market_id, market_date, event_time, as_of_seq, items`。每项返回 `symbol,name,sector,initial_price_cents,prev_close_cents,last_price_cents,last_trade_time,open_cents,high_cents,low_cents,volume,turnover_cents,change_pct`。无成交时展示有效价可用初始价，日内 OHLC 返回 null |
| `GET /events` | `after_seq` 默认 `0`，返回其后事件。`until_seq` 默认当前高水位，显式上界取请求值和高水位的较小者。`limit` 默认 1000、最大 5000。响应 `items,until_seq,next_after_seq,has_more`，客户端翻页沿用返回的上界。当前保留最早序号之前的游标返回 410，正好等于 `first_retained_seq - 1` 可读 |
| `GET /bars` | 参数 `symbol` 必填，周期只接收 `1m,5m,1d`，默认 `1m`。`to` 默认当前模拟时间后一秒，`from` 默认 `to - 24 小时`。范围最长 30 天，按 K 线起始时间过滤，最多 2000 根，超限返回 400 并提示缩窄范围。用已提交时间作水位，未来的 `to` 不会生成未来数据。返回 `items`，每个元素含 `start_time,end_time,open_cents,high_cents,low_cents,close_cents,volume,turnover_cents,trade_count,complete` |
| `GET /sectors` | 响应 `as_of_seq, items, market`。板块条目含 `sector,float_cap_cents,change_pct,up,down,flat,no_trades`，`market` 用同一公式计算全市场。按板块名称排序。无当日成交的标的计入 `no_trades` |
| `WebSocket /v1/stream` | URL 必填 `market_id` 与 `after_seq`。市场和游标在升级连接前验证。只发全市场事件。连接每 5 秒发 JSON 心跳，发送超时为 3 秒，WebSocket ping 每 15 秒、pong 超时 5 秒。连接保持只读，拒收客户端业务消息 |

接口错误统一为 `{"error":{"code":"cursor_expired","message":"游标早于可查询范围","details":{"first_retained_seq":"1200"}}}` 结构。400 用于格式、范围或未来游标，404 用于未知市场或标的，409 用于实时续订积压超过 1024 条并引导客户端先走历史接口，410 用于保留期缺口，503 用于数据库或订阅容量不可用。统一 JSON `Content-Type`，错误中不回显原始数据库连接信息。只支持同源浏览器连接及本地程序访问。

WebSocket 升级前先在分发器注册连接并开始缓存，在读事务中获取高水位 `H`，按序读齐 `(after_seq,H]`，并确认尾段未超上限。升级后第一帧 `{"type":"subscribed","market_id":"one-piece-dev","replay_until_seq":"1212"}`，然后依次发送历史和缓冲流，丢弃所有序号小于等于 H 的缓冲事件。客户端每次只以最后连续消费的业务序号续订。心跳不分配事件序号，含已提交的 `last_event_seq, logical_tick, server_time`。发送队列溢出使用关闭码 1013，客户端走历史补齐后重连。发现正在使用的分发序号已经低于保留边界时，断开其订阅并重置分发游标，客户端重新拉快照。

`trade` 事件使用第 6 节示例结构，并带 `market_date` 和 `generated_at`。`session` 事件结构相同但不含 `symbol,price_cents,quantity,turnover_cents`，表示新交易日开始。已发生事件不做修改。成交撤销、行情修订和订单簿需要另定事件类型和对应版本。

## 13. 执行顺序、自动验证及完成条件

1. 建立迁移、数据构建、12 标的开发数据和初始化。用单元测试确认角色匹配、别名、缺失、重复、权重、流通比例、随机边界、固定种子重现、零成交和跨日。
2. 实现交易步及 PostgreSQL 读写。通过集成测试验证一个逻辑步只提交一次、K 线可由逐笔重算、逐笔清理水位正确、并发生成器被同市场锁拒绝。为测试设提交前、提交后但发送通知前两个故障注入点，故障注入保留在测试内部。
3. 实现 REST、WebSocket、健康检查、日志和退出。集成测试覆盖快照与订阅之间持续成交、断线补齐、数据库短暂停机和恢复、客户端落后超过保留期、慢订阅断开。所需的等待使用有截止时间的轮询或信号。时间测试注入时钟或固定起点，避免等到真实午夜。
4. 运行容器测试。`Dockerfile` 提供 `test` 构建阶段执行 `go vet ./...` 和 `go test -race ./...`。Compose 增设只在 profile `test` 启动的测试运行器，使用独立测试数据库，执行集成测试。`verify` 可在运行镜像中从 HTTP 及 WebSocket 验证真实部署。
5. 构建正式标的发布文件并运行全量验收。合成压力数据用于验证规模与分发，正式数据报告用于确认角色覆盖和资料来源，两项都要写入实施结果。质量门槛为可核验的命令输出和失败说明。任何必验项失败须定位并修复后重跑，数据源不可获取时按第 10.2 节记录发布缺口。

同时交付独立的 `compose.scale.yaml`。压力市场固定为 `one-piece-scale`，Compose 项目名固定为 `market-simulator-scale`，宿主端口用 8788，仍只绑定回环地址。合成数据由构建过程生成，规模固定为 1,500 个标的。正式数据使用独立的 `compose.full.yaml`，项目名固定为 `market-simulator-full`，市场为 `one-piece-c1198-v1`，回环端口 8789。把 `./data/release/one-piece.json` 只读挂载为 `/app/data/one-piece.json` 并指定 `DATASET_PATH`。正式和压力市场都使用默认 2% 成交概率。测试命令仅操作各自的卷。

文档中规定将来实现后的本机验收命令。测试从模拟系统目录执行。

~~~sh
docker build --target test .
docker compose --profile test run --rm integration
docker compose up -d --build --wait
docker compose exec -T market /app/market-simulator verify --base-url http://127.0.0.1:8787 --market one-piece-dev --profile smoke
docker compose restart market
docker compose exec -T market /app/market-simulator verify --base-url http://127.0.0.1:8787 --market one-piece-dev --profile smoke
docker compose -p market-simulator-scale -f compose.scale.yaml up -d --build --wait
docker compose -p market-simulator-scale -f compose.scale.yaml exec -T market /app/market-simulator verify --base-url http://127.0.0.1:8787 --market one-piece-scale --profile scale
docker compose -p market-simulator-scale -f compose.scale.yaml down
~~~

正式来源的构建和部署使用以下命令。发布目录由构建入口创建，`data/overrides.json` 随项目提交。

~~~sh
go run ./cmd/market-simulator dataset fetch --out-dir data/raw
go run ./cmd/market-simulator dataset build --characters data/raw/characters.html --appearances data/raw/appearances.csv --rankings data/raw/rankings.html --source-manifest data/raw/sources.json --cutoff-chapter 1198 --overrides data/overrides.json --out data/release/one-piece.json
docker compose -p market-simulator-full -f compose.full.yaml up -d --build --wait
docker compose -p market-simulator-full -f compose.full.yaml exec -T market /app/market-simulator verify --base-url http://127.0.0.1:8787 --market one-piece-c1198-v1 --profile smoke
~~~

| 验收入口 | 机械判定 |
| --- | --- |
| `verify --profile ready` | 读取 `/readyz`，仅 HTTP 200 时退出码为零。设置两秒请求超时。供 Compose 健康检查调用 |
| `verify --profile smoke` | 在 20 秒内就绪，不建订阅等待成交水位上涨，接口读取逐笔、快照、板块、K 线。通过 WebSocket 从快照序号起消费至少 5 条连续事件。断开一次再从上一序号补齐，与 REST 中相同 seq 的成交完全一致。每项失败退出码非零 |
| `integration` 中的重启用例 | 保存 tick、最后序号以及最后一根 K 线。在同一库重启引擎，等待 tick 增大且至少一条新事件出现。回读原事件区间确认未重复。新事件第一条等于旧高水位加一。从事件累加 K 线校验量额 |
| `verify --profile scale` | 使用 1,500 标的、每步 2% 成交概率、每步真实等待 1 秒，10 个并发订阅者持续消费 60 秒。检查序号连续，持续记录请求和写入 p95、已提交与已发送的差值、数据库表体积。初始目标为写入 p95 小于 250 毫秒、产生到收到事件 p95 小于 1 秒，结束后 5 秒内追平。记录硬件与实际数字 |
| 正式数据发布 | 给出源文件指纹、冻结章节、候选数、实际标的数、分阵营数、出场和人气匹配数及未解歧义。取三个高频角色和两个歧义案例验证初始化及数据口径。以正式数据启动同一构建，验证通用 Smoke 流程 |

交付包括源码、锁定依赖、Compose、迁移、开发数据、正式数据构建入口与数据报告，以及含日期和实际输出的验收记录。运行所需环境依赖在 README 给出。服务设计未覆盖账户、策略执行和实盘交易，这些可独立消费已发布行情接口。

本文件为 VoyageFeed 独立仓库的设计副本，接手入口见 [AGENTS.md](AGENTS.md)。
