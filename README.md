# VoyageFeed（航路行情）

模拟证券行情服务：一个 Go 进程持续生成《海贼王》角色标的的模拟逐笔成交，提供 REST 查询与 WebSocket 推送，PostgreSQL 17 持久化。**这是模拟行情，不能交易**——没有账户、委托、撮合和实盘接入。

## 目录与架构

```text
cmd/market-simulator/   入口：serve（运行）、dataset（数据构建）、verify（验收）
engine/                 随机成交生成：按逻辑时钟每步对每只标的独立抽样
store/                  PostgreSQL 读写：一步一事务、逐笔/快照/K 线同事务提交
httpapi/                REST 查询、WebSocket 接入、健康检查与监控
stream/                 订阅分发：水位跟踪、断线重连协议
dataset/                角色资料解析与发布文件构建（资料只作用于初始化和分类）
data/                   发布文件（dev.json / scale.json / release/one-piece.json）
compose.yaml            开发环境（one-piece-dev，端口 8787）
compose.full.yaml       正式环境（one-piece-c1198-v2，端口 8789）
compose.scale.yaml      压测环境（one-piece-scale，1,500 标的，端口 8788）
```

角色数据只决定初始标的池、初始价格和板块分类；市场启动后每只标的的涨跌独立随机，与其人气和剧情无关。引擎是随机生成成交，不是撮合。

## 快速开始

只运行服务只需要 Docker。用到下面两类工具时的已知坑（详见 PLAN「环境与依赖」节）：主机 `go` 不在 PATH 时用全路径 `/usr/local/go/bin/go`；Docker Hub 拉取超时先配镜像加速器，Go 模块用 `GOPROXY=https://goproxy.cn,direct`。WebSocket 客户端获取方式见「查询与订阅」。

```sh
docker compose up -d --build --wait     # 起开发环境（数据库 + 行情进程）
docker compose ps                       # 两个容器都应为 healthy
curl http://127.0.0.1:8787/readyz       # 返回 ok
```

停止并保留数据卷（**`down -v` 会删掉已有行情，不要用**）：

```sh
docker compose down
```

再次 `up` 复用同一个卷和市场，行情从停机时的水位继续，停机期间模拟时间暂停、不追真实时间。

查看日志与重启：

```sh
docker compose logs -f --tail 100 market   # 行情进程日志
docker compose restart market              # 重启行情进程，数据库卷与水位保留
```

备份与恢复：行情数据在命名卷（如 `voyage-feed_db-data`）里，备份前先 `docker compose stop market` 再对卷做备份（pg_dump 或卷级备份）。**恢复较早的备份会使事件序号回退，必须发布新的 `MARKET_ID` 重建市场，不能原 ID 覆盖**（见 DESIGN §7.1）。

## 查询与订阅

```sh
# 市场状态（logical_tick 和 last_event_seq 持续前进）
curl http://127.0.0.1:8787/v1/markets/one-piece-dev/status

# 快照 / 逐笔 / 板块 / K 线
curl http://127.0.0.1:8787/v1/markets/one-piece-dev/snapshot
curl 'http://127.0.0.1:8787/v1/markets/one-piece-dev/events?after_seq=0&limit=5'
curl http://127.0.0.1:8787/v1/markets/one-piece-dev/sectors
curl 'http://127.0.0.1:8787/v1/markets/one-piece-dev/bars?symbol=D14001F4649BOP&interval=1m'
```

按名称找标的（接口不支持按名称过滤，取列表后本地搜；列表按 `next_after_symbol` 翻页）：

```sh
curl -s http://127.0.0.1:8787/v1/markets/one-piece-dev/instruments   | jq '.items[] | select(.name | test("路飞")) | {symbol, name, sector}'
```

WebSocket 订阅（用 `websocat`，或任何 WebSocket 客户端；两者都没有时见下方浏览器备选）：

```sh
websocat 'ws://127.0.0.1:8787/v1/stream?market_id=one-piece-dev&after_seq=0'
```

`websocat` 不是预装工具：Ubuntu 23.04+ 可 `sudo apt install websocat`，22.04 默认源没有此包，从 [GitHub Releases](https://github.com/vi/websocat/releases) 下载静态二进制（`x86_64-unknown-linux-musl`，`chmod +x` 后移入 PATH）。

不装任何东西的浏览器备选：先在浏览器打开 `http://127.0.0.1:8787/readyz`（看到 `ok` 说明浏览器可达服务；别在 https 页面的控制台连 `ws://`，会被拦截），按 F12 在控制台粘贴：

```js
let last = 0;
const ws = new WebSocket('ws://127.0.0.1:8787/v1/stream?market_id=one-piece-dev&after_seq=0');
ws.onmessage = e => { const m = JSON.parse(e.data); console.log(m); if (m.event_seq) last = Number(m.event_seq); };
ws.onclose = e => console.log('关闭码', e.code, '最后连续序号', last);
```

主动断线用 `ws.close()`；重连时把 `last` 填回 `after_seq` 再执行一遍。连接每 5 秒发心跳（不占序号、携带已提交水位），长时间无成交也可用它判活；心跳里的序号可能领先你实收的，属正常。

连接后先收 `subscribed`（含 `replay_until_seq`），再收补发与实时业务事件。断开后用**最后连续消费的业务序号**重连续订；服务端返回 1013 关闭码时（可能是队列满或恢复断开，不依赖关闭文案判断），同样按最后连续序号重连。游标早于保留边界得到 410，需重新取快照再订阅；缺口超过 1024 得到 409，先用 `/events` 补历史。停止/重启期间新订阅返回 503 `shutting_down`，属可重试错误，退避重试并以 `/readyz` 通过为恢复标志。

## 数据口径（重要）

- **两类时间**：`event_time` 是模拟时钟（整秒，一个逻辑步推进模拟时间 1 秒）；`generated_at` 是服务器墙上时间（带毫秒小数），测延迟用它；心跳与 `/status` 里的 `server_time` 是同源墙钟，用于对时。研究计时用 `event_time` 和 `logical_tick`，不要解析 `generated_at` 的固定格式长度。
- **市场全天运行**，按北京时间零点切换模拟日，没有开盘、午休和收盘。不是每秒都有成交；单只标的几十秒没有成交是正常现象，不是停摆。
- **空最新价**表示该标的尚未成交，参考快照里的参考价，不是停牌。
- **翻页与追平**：`/events` 的 `has_more` 只表示查询窗口内还有下一页，不是追平信号；晚于已提交序号的 `until_seq` 返回 400。分页用返回的 `next_after_seq` 续读。
- **序号顺序**：`event_seq` 全市场连续递增；同一 `logical_tick` 内按 `instrument_id` 排序，不代表跨标的的先后。
- **交易代码**是 12 位大写十六进制加 `OP` 后缀（如开发市场路飞 `D14001F4649BOP`）；DESIGN 第 6 节示例中的 `OP000123` 仅为占位，不是实际格式。
- **成交概率**：开发市场每标的每步 2000/10000；正式市场与压测市场均为 200/10000。三个环境的成交密度不同，不能混成一个收益样本。
- **手数**：每笔 1–10 手、每手 100 股，与价格和流通股独立。换手率和量价关系不是信息。
- **逐笔保留**：默认保留 7 个模拟日，过期删除；K 线长期保留，过期后不能用逐笔回算。过期游标订阅得 410，重新取快照再订。
- **比率字段**：`change_pct` 等是比率（0.01 = 1%），字段单位详见 DESIGN 第 12 节。
- **K 线查询**：`to` 只按 K 线起点筛选，返回周期的 `end_time` 可以晚于 `to`；未走完的周期 `complete=false`，其字段可能被后续请求改写，研究回测只用 `complete=true` 的周期。
- **流通比例**按角色身份近似：阵营负责人 30%、核心成员 50%、普通成员 70%、未知 50%。这是模型假设，不是交易所的自由流通盘。
- **初始条件是构造性的**：初始价（人气档）、初始市值（章节数权重）与板块（阵营）在启动时由人物资料确定性生成，三者的截面相关不是市场信息；启动后的一切变动才是随机的。
- **这些不是故障**：市场全天运行没有开收盘；单只标的几十秒无成交、订阅流只见心跳是正常；「最新价」为空只是尚未成交，看参考价；停止/重启期间 503 `shutting_down` 属预期，退避重试、`/readyz` 通过即恢复；重启后从停机水位继续、模拟时间暂停不补涨；服务只绑本机（127.0.0.1），别的设备打不开是设计不是坏了。

## 环境与验收

三环境宿主端口 8787/8788/8789 都只绑定 127.0.0.1 回环、外部不可达；下文 verify 命令里的 8787 是容器内视角。自动验收未覆盖的场景清单见 RECORD 2026-11-19 11:20 补注。

环境准备（镜像、代理、已知环境坑）见 [PLAN.md](PLAN.md) 的「环境与依赖（跨机器同步用）」章节。

```sh
# 单元测试（不连数据库）
go vet ./... && go test -race ./...

# 容器内测试构建（vet + race 单测）
docker build --target test .

# 独立数据库集成测试（两步分包，运行器已内置）
docker compose --profile test build integration
docker compose --profile test run --rm integration

# 开发 Smoke（水位上涨、快照/逐笔/K 线、断线重连、REST 对照）
docker compose exec -T market /app/market-simulator verify \
  --base-url http://127.0.0.1:8787 --market one-piece-dev --profile smoke

# 正式环境与压测环境（独立端口与数据卷）
docker compose -p market-simulator-full -f compose.full.yaml up -d --build --wait
docker compose -p market-simulator-full -f compose.full.yaml exec -T market \
  /app/market-simulator verify --base-url http://127.0.0.1:8787 \
  --market one-piece-c1198-v2 --profile smoke

docker compose -p market-simulator-scale -f compose.scale.yaml up -d --build --wait
docker compose -p market-simulator-scale -f compose.scale.yaml exec -T market \
  /app/market-simulator verify --base-url http://127.0.0.1:8787 \
  --market one-piece-scale --profile scale   # 60 秒十路订阅压测
```

集成测试需要 `DATABASE_URL` 与 `ADMIN_DATABASE_URL` 两个环境变量（compose 运行器已内置）；宿主直跑时一律加 `-count=1`。验收通过条件以各命令退出码为准。

## 数据重建

```sh
# 抓取上游三源到 data/raw（网络不可达时可离线提供同格式文件）
go run ./cmd/market-simulator dataset fetch --out-dir data/raw

# 以覆盖文件重建正式发布（数据变化时须换新的 MARKET_ID）
go run ./cmd/market-simulator dataset build \
  --characters data/raw/characters.html \
  --appearances data/raw/appearances.csv \
  --rankings data/raw/rankings.html \
  --source-manifest data/raw/sources.json \
  --cutoff-chapter 1198 \
  --overrides data/overrides.json \
  --out data/release/one-piece.json
```

换市场时使用新的 `MARKET_ID`（`compose.full.yaml` 的环境变量与 healthcheck 两处同步），不要修改已有市场的种子或数据集；历史市场数据保留在同一个库里。正式板块标签多为自动归类，仅 17 个白名单板块经人工核对（68 条身份补录），其余 234 个板块含已知噪声、未经逐一验收，不作为研究依据（详见 RECORD 2026-11-19 12:06）。

容量参考：`market_events` 稳态约为「事件速率 × 7 × 86400」行（正式约 1850 万行）；`bars_1m` 无保留上限，正式约每分钟填充率 70% 计 ≈155 万行/日。磁盘监控用 `/metrics` 的 `market_db_table_bytes`。

更多接口字段与参数详见 [DESIGN.md](DESIGN.md)；实施过程见 [RECORD.md](RECORD.md)。
