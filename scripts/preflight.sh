#!/bin/sh
# VoyageFeed 环境预检：换机首跑或 S4.6 复验前执行。输出 检查项/结果/建议 三列。
# 只读检查，唯一 sudo 命令（iptables 探针）失败时跳过并提示，不自动执行任何修复。
set -u
PASS=0; FAIL=0
ok()   { printf '%-28s %-6s %s\n' "$1" OK "$2"; PASS=$((PASS+1)); }
bad()  { printf '%-28s %-6s %s\n' "$1" FAIL "$2"; FAIL=$((FAIL+1)); }
info() { printf '%-28s %-6s %s\n' "$1" INFO "$2"; }

echo "== VoyageFeed preflight $(date '+%F %T') =="

# 1 工具链
GO=""
for c in go /usr/local/go/bin/go; do command -v "$c" >/dev/null 2>&1 && GO="$c" && break; done
[ -n "$GO" ] && ok "go" "$($GO version 2>/dev/null | cut -d' ' -f3)（$GO）" || bad "go" "未找到；装 Go 1.25 或用 /usr/local/go/bin/go"
docker version --format '{{.Server.Version}}' >/dev/null 2>&1 && ok "docker" "server $(docker version --format '{{.Server.Version}}')" || bad "docker" "Docker Engine 不可用"
docker compose version >/dev/null 2>&1 && ok "compose" "$(docker compose version --short)" || bad "compose" "Compose v2 不可用"

# 2 镜像
for img in postgres:17 golang:1.25.14-bookworm debian:bookworm-slim; do
  docker image inspect "$img" >/dev/null 2>&1 && ok "镜像 $img" 本地存在 || bad "镜像 $img" "缺失；Docker Hub 超时先配镜像加速器再 docker pull"
done

# 3 iptables-legacy 探针（需要 sudo；不可用则提示人工）
OUT=$(sudo -n iptables -S FORWARD 2>&1)
if echo "$OUT" | grep -q 'iptables-legacy tables present'; then
  bad "iptables-legacy" "命中已知坑：legacy 残留链会杀容器间流量；修复见 PLAN 环境坑第 1 条"
elif echo "$OUT" | grep -q '^-P FORWARD'; then
  ok "iptables-legacy" "无 legacy 残留告警"
else
  info "iptables-legacy" "sudo 不可用，人工执行：sudo iptables -S FORWARD 查 Warning 字样"
fi

# 4 容器网络连通探针（临时网络 + pg_isready）
NET="vf-preflight-$$"
if docker network create "$NET" >/dev/null 2>&1; then
  CID=$(docker run -d --rm --name "vf-pf-pg-$$" --network "$NET" -e POSTGRES_PASSWORD=p -e POSTGRES_DB=t postgres:17 2>/dev/null | cut -c1-12)
  if [ -n "$CID" ]; then
    sleep 8
    if docker run --rm --network "$NET" postgres:17 sh -c "timeout 6 pg_isready -h vf-pf-pg-$$ -p 5432 -U postgres 2>/dev/null" | grep -q 'accepting connections'; then
      ok "容器间网络" "pg_isready accepting connections"
    else
      bad "容器间网络" "不通；高度怀疑 iptables-legacy 坑（见第 3 项与 PLAN）"
    fi
    docker rm -f "vf-pf-pg-$$" >/dev/null 2>&1
  else
    bad "容器间网络" "探针容器启动失败"
  fi
  docker network rm "$NET" >/dev/null 2>&1
else
  info "容器间网络" "无法创建临时网络，跳过"
fi

# 5 持久目录与磁盘
touch /var/tmp/.vf-preflight 2>/dev/null && rm -f /var/tmp/.vf-preflight && ok "/var/tmp" 可写 || bad "/var/tmp" 不可写
DF=$(df -Pm /var/tmp 2>/dev/null | awk 'NR==2{print $4}')
[ -n "$DF" ] && [ "$DF" -gt 4096 ] && ok "磁盘" "/var/tmp 剩余 ${DF}MB（>4GB）" || info "磁盘" "剩余 ${DF:-?}MB，压测与长期运行建议 >4GB（bars_1m 见 README 容量参考）"

# 6 命名卷
for v in voyage-feed_db-data voyage-feed_test-db-data market-simulator-scale_db-data market-simulator-full_db-data; do
  docker volume inspect "$v" >/dev/null 2>&1 && info "卷 $v" "存在（按 PLAN 数据卷现状核对基线）" || info "卷 $v" "不存在（新基线将创建）"
done
ANON=$(docker volume ls -q | grep -cvE '^(voyage-feed|market-simulator)' || true)
[ "${ANON:-0}" -gt 0 ] && info "匿名卷" "发现 ${ANON} 个非项目卷，清理需单独授权" || ok "匿名卷" 无

echo "== 结果：$PASS 通过 / $FAIL 失败 =="
[ "$FAIL" -eq 0 ]
