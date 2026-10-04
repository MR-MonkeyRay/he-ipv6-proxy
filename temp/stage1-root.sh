#!/bin/bash
# stage1-root.sh — 阶段 1 中唯一需要 root 的部分：确定可用出站池、安装 AnyIP local 路由、
# 然后以完整链路做端到端可行性验证（doctor 实拨 + 代理实拨三次看源地址是否轮换）。
#
# 用法:
#   sudo ./stage1-root.sh                    # 自动按候选顺序探测
#   sudo ./stage1-root.sh 2001:470:8:34d::/64   # 指定 HE 门户里的 "Routed /64"
#
# 幂等，可重复执行。失败候选留下的 local 路由会被清理。
# 退出码: 0 = 池可用且端到端验证通过；1 = 所有候选都拨不通。

set -u

here=$(cd "$(dirname "$0")" && pwd -P)   # -P: 解析符号链接(如 /home/x/workspace → /data/x/workspace)
LP="$here/light-proxy"
CFG="$here/config.toml"
PROBE="$here/config.probe.toml"
LOG="$here/proxy.root-run.log"
ECHO_URL='http://api6.ipify.org/'
ECHO_URL_TLS='https://api6.ipify.org/'   # 走 CONNECT 隧道, 回显隧道内的源地址

[ "$(id -u)" -eq 0 ] || { echo "请用 sudo 运行: sudo $0"; exit 1; }
[ -x "$LP" ] || { echo "$LP 不存在或不可执行，请先在项目根目录 go build"; exit 1; }

# TOML 取值: [^"]* 到第一个收尾引号为止, 再吞掉行尾注释/空白 (配置里常带行尾注释)
user=$(sed -n 's/^user *= *"\([^"]*\)".*/\1/p' "$CFG")
pass=$(sed -n 's/^pass *= *"\([^"]*\)".*/\1/p' "$CFG")
[ -n "$user" ] && [ -n "$pass" ] || { echo "无法从 $CFG 读出 auth.user/auth.pass"; exit 1; }

listen=$(sed -n 's/^listen *= *"\([^"]*\)".*/\1/p' "$CFG")
base_port=${listen##*:}
case "$base_port" in
  ''|*[!0-9]*) echo "无法从 listen = \"$listen\" 解析端口"; exit 1 ;;
esac
# 临时实例用 base+1..base+20 里的第一个空闲端口: 正在运行的常驻实例/别的服务占着 base 时也能探测
probe_port=""
for off in $(seq 1 20); do
  cand=$((base_port+off))
  if [ -z "$(ss -ltn 2>/dev/null | awk '{print $4}' | sed -n "/:$cand\$/p" | sed -n '1p')" ]; then
    probe_port=$cand
    break
  fi
done
[ -n "$probe_port" ] || { echo "找不到空闲探测端口 (base $base_port)"; exit 1; }
echo "配置监听 $listen → 临时实例用 127.0.0.1:$probe_port"
proxy="http://$user:$pass@127.0.0.1:$probe_port"

# ---- 1. 确定可用池 ---------------------------------------------------------
# 候选顺序: 命令行参数 → HE 惯例（链路 /64 的第三段 +1，本例 2001:470:7:34d → 2001:470:8:34d）
#           → 隧道链路 /64 本身（兜底）
cands=()
[ $# -ge 1 ] && cands+=("$1")
cands+=("2001:470:8:34d::/64" "2001:470:7:34d::/64")

winner=""
for p in "${cands[@]}"; do
  printf '\n=== 探测池 %s （实拨超时 4s）\n' "$p"
  ip -6 route replace local "$p" dev lo
  sed -e "s|^ipv6_pool = .*|ipv6_pool = \"$p\"|" \
      -e 's|^dial_timeout = .*|dial_timeout = "4s"|' \
      -e "s|^listen = .*|listen = \"127.0.0.1:$probe_port\"|" \
      -e 's|^file = .*|file = ""|' "$CFG" > "$PROBE"   # 探测实例只写 stdout(下面重定向到 $LOG), 不污染常驻实例的日志文件
  chmod 0644 "$PROBE"   # 临时实例可能以普通用户身份运行, 必须可读
  if "$LP" doctor -c "$PROBE"; then
    winner="$p"
    break
  fi
  ip -6 route del local "$p" dev lo 2>/dev/null
done

if [ -z "$winner" ]; then
  cat <<EOF

[FAIL] 所有候选前缀都拨不通（绑得出包、回程没回来）。
       1) 到 HE 门户 Tunnel Details 抄 "Routed IPv6 Prefixes → Routed /64"，
          然后重跑: sudo $0 <routed-prefix>/64
       2) 若门户给的值仍失败，检查 nft/iptables 是否丢弃 proto 41，以及门户里隧道是否 Up。
EOF
  exit 1
fi

sed -i "s|^ipv6_pool = .*|ipv6_pool = \"$winner\"|" "$CFG"
printf '\n[OK] 可用出站池 = %s（已回写 %s）\n' "$winner" "$CFG"
if [ "$winner" != "2001:470:8:34d::/64" ]; then
  echo "[注意] 命中的不是惯例候选，请与门户里的 Routed /64 核对一次。"
fi

printf '\n=== AnyIP local 路由（内核 local 表）\n'
ip -6 route show table local | sed -n "\|local ${winner}|p"

# 用胜出的池 + 临时端口生成探测配置, 供下面的临时实例使用(不改动 $CFG 的 listen)
sed -e "s|^ipv6_pool = .*|ipv6_pool = \"$winner\"|" \
    -e "s|^listen = .*|listen = \"127.0.0.1:$probe_port\"|" \
    -e 's|^file = .*|file = ""|' "$CFG" > "$PROBE"
chmod 0644 "$PROBE"   # 临时实例以普通用户身份运行, 必须可读

# ---- 2. 端到端验证：起服务 + 实拨 ------------------------------------------
printf '\n=== 启动服务（临时实例，验证完自动停止）\n'
# 以普通用户身份跑临时实例（与 systemd 服务一致；FREEBIND 免 root），拿不到用户名时退回 root。
runas=${SUDO_USER:-}
if [ -n "$runas" ] && [ "$runas" != root ] && id -u "$runas" >/dev/null 2>&1 \
   && command -v runuser >/dev/null 2>&1; then
  printf '  (以 %s 身份运行)\n' "$runas"
  runuser -u "$runas" -- "$LP" run -c "$PROBE" > "$LOG" 2>&1 &
else
  "$LP" run -c "$PROBE" > "$LOG" 2>&1 &
fi
pid=$!
trap 'kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; rm -f "$PROBE"' EXIT
for _ in $(seq 1 60); do
  curl -s -o /dev/null -m1 "http://127.0.0.1:$probe_port/" && break
  sleep 0.25
done

code_noauth=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -x "http://127.0.0.1:$probe_port" "$ECHO_URL")
code_v4only=$(curl -s -o /dev/null -w '%{http_code}' -m 10 -x "$proxy" "http://23.94.112.249/")

s1=$(curl -s -m 20 -x "$proxy" "$ECHO_URL")
s2=$(curl -s -m 20 -x "$proxy" "$ECHO_URL")
s3=$(curl -s -m 20 -x "$proxy" "$ECHO_URL")

printf '\n--- 结果\n'
printf '无凭据请求          : HTTP %s   (期望 407)\n' "$code_noauth"
printf 'IPv4-only 目标      : HTTP %s   (期望 502，纯 IPv6 出站不回退)\n' "$code_v4only"
printf '三次实拨的目标侧源地址:\n  %s\n  %s\n  %s\n' "$s1" "$s2" "$s3"

verdict=0
for s in "$s1" "$s2" "$s3"; do
  case "$(ip -6 route get "$s" 2>/dev/null)" in
    *"dev lo"*) ;;
    *) printf '[FAIL] %s 不在 %s 的 AnyIP 路由内\n' "$s" "$winner"; verdict=1 ;;
  esac
done
[ "$s1" != "$s2" ] && [ "$s2" != "$s3" ] && [ "$s1" != "$s3" ] || {
  printf '[FAIL] 三次源地址不唯一\n'; verdict=1;
}
# CONNECT 隧道: 必须用 -v 才看得到握手行 (-s 会把它连 curl 的错误信息一起吞掉)
conn_out=$(curl -sv -m 15 -x "$proxy" "$ECHO_URL_TLS" 2>&1); conn_rc=$?
t1=$(curl -s -m 20 -x "$proxy" "$ECHO_URL_TLS")
t2=$(curl -s -m 20 -x "$proxy" "$ECHO_URL_TLS")
case "$conn_out" in
  *'200 Connection Established'*) ;;
  *) printf '[FAIL] CONNECT 隧道未建立 (curl exit=%s), curl 输出:\n' "$conn_rc"
     printf '%s\n' "$conn_out" | sed -n '/CONNECT/p;/HTTP\/1.1/p;/curl:/p' | sed -n '1,6p'
     verdict=1 ;;
esac
printf 'CONNECT 隧道内源地址:\n  %s\n  %s\n' "$t1" "$t2"
for t in "$t1" "$t2"; do
  case "$(ip -6 route get "$t" 2>/dev/null)" in
    *"dev lo"*) ;;
    *) printf '[FAIL] %s 不在 %s 的 AnyIP 路由内\n' "$t" "$winner"; verdict=1 ;;
  esac
done
[ "$t1" != "$t2" ] || { printf '[FAIL] 两条 CONNECT 隧道的源地址相同\n'; verdict=1; }

printf '\n=== 服务日志（含每次拨号的 src=）\n'
tail -n 8 "$LOG"

if [ "$verdict" -eq 0 ]; then
  cat <<EOF

[PASS] 阶段 1 端到端验证通过：池 $winner 上绑定/回程/轮换全部成立。
       已保留 AnyIP local 路由（ip -6 route add local $winner dev lo，重启后需重建）。
       下一步：sudo ./install-systemd.sh —— 三个 unit 直接执行 temp/ 下的二进制与配置，
       不复制任何程序文件到系统目录，开机自启 + 自动重建隧道与 local 路由。
EOF
else
  echo "[FAIL] 见上面的失败项。"
fi
exit "$verdict"
