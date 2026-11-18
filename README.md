# VoyageFeed（航路行情）

模拟证券市场的行情服务。当前可以启动 PostgreSQL 17，并用离线夹具构建开发样本。行情进程的配置已经写进 Compose，这一步还没有启动它。

## 当前能做什么

- 用 Docker Compose 启动数据库，数据留在命名卷里。数据库里可以有开发市场 `one-piece-dev` 的初始标的，逻辑时钟停在 0。
- `dataset build` 可以从本地夹具生成发布文件。`dataset fetch` 会访问上游，本机验收不依赖它成功。
- `serve` 会迁移、复用已有市场并提供查询和 WebSocket。没有 `DATABASE_URL` 时退出码为 2。`verify --profile scale` 仍退出码 2。
- 开发市场 `one-piece-dev` 仍停在逻辑时钟 0。不要在这一步执行 `docker compose up` 来启动 `market`，否则会推进这个市场。手动启动留到后续验收。

开发样本 `data/dev.json` 的出场次数是测试夹具里的小整数，不是漫画统计。样本包含草帽一伙、海军、其他三个阵营。乌索普和山治名次都是 11。布鲁克没有排名，青雉没有出场次数，开发夹具路人的出场次数是明确的 0。

## 环境

在 WSL Ubuntu 中准备：

- Docker Engine 与 Docker Compose
- Go 1.25，用于本机 `go test`
- 不在宿主机安装 PostgreSQL

数据库只留在 Compose 网络内，不把 5432 发布到宿主机。行情服务的 Compose 端口是 `127.0.0.1:8787:8787`。本步没有启动该容器，宿主机 8787 仍不应监听。

## 启动数据库

在仓库根目录执行：

```sh
docker compose up -d --wait
docker compose ps
docker compose exec -T db pg_isready -U voyage -d voyage
docker compose port db 5432
```

`up` 由 Compose 拉取 `postgres:17`。`db` 应为 healthy。`docker compose ps` 的 PORTS 只显示容器内的 `5432/tcp`，不应出现 `127.0.0.1:5432` 或 `0.0.0.0:5432`。Compose v5.8.0 上 `docker compose port db 5432` 会打印 `:0` 且退出码为 0，这不表示端口已经发布到宿主机。

不复制 `.env` 也能启动。数据库用户和库名都是 `voyage`。口令来自环境变量 `POSTGRES_PASSWORD`，未设置时使用 `.env.example` 里的本机占位。日志和记录不要粘贴口令或 `docker compose config` 全文。

停止并保留数据卷：

```sh
docker compose down
```

再次 `docker compose up -d --wait` 会使用同一个命名卷。只有明确要丢掉这份还没有应用表的空库时，才使用 `docker compose down -v`。

## 本机测试

```sh
go vet ./...
go test ./...
```

这些测试不连接数据库。镜像里的 `test` 阶段留给后续第一次构建：`docker build --target test .`。本步不把构建镜像当作通过条件。

## 开发样本

离线重建开发样本：

```sh
go run ./cmd/market-simulator dataset build \
  --characters data/dev-src/characters.html \
  --appearances data/dev-src/appearances.csv \
  --rankings data/dev-src/rankings.html \
  --source-manifest data/dev-src/sources.json \
  --cutoff-chapter 10 \
  --overrides data/dev-src/overrides.json \
  --out data/dev.json
```

正式覆盖文件 `data/overrides.json` 目前是空对象。上游原始页面放在 `data/raw/`，不入库。

## 命令

```sh
go run ./cmd/market-simulator serve
go run ./cmd/market-simulator verify --profile ready
```

`serve` 需要 `DATABASE_URL`。指向库 `voyage` 会推进 `one-piece-dev`，手动验收之前不要这样做。`dataset fetch --out-dir data/raw` 会访问公开来源；抓取失败不影响上面的离线样本。

暂停生成可以停掉 `market` 容器，数据库卷还在。查看日志用 `docker compose logs market`。换市场时使用新的 `MARKET_ID`，不要改已有市场的种子或数据集。备份是保留 Compose 命名卷；恢复早期备份时要换新的市场 ID。这些容器命令留到手动验收时再执行。
