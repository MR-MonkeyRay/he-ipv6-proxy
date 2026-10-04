#!/bin/bash
# install-systemd.sh — 把「HE 6in4 隧道 + AnyIP local 路由 + light-proxy」装成开机自启的 systemd 服务。
#
# 特点:
#   * 不复制任何程序文件到系统目录: systemd 直接执行 temp/ 下的 light-proxy,
#     配置读 temp/config.toml。系统目录里只落 3 个 unit 声明文件(systemd 只能从
#     /etc/systemd/system 读系统服务, 这是无法绕开的)。
#   * 不新建用户: 代理以执行 sudo 的普通用户身份运行(代理不需要任何特权)。
#
# 用法:
#   cd /data/monkeyray/workspace/light-proxy/temp
#   sudo ./install-systemd.sh                 # 服务以执行 sudo 的普通用户身份运行
#   sudo ./install-systemd.sh <username>      # 显式指定运行用户
#
# 幂等: 可重复执行(重装 unit 并重启服务)。退出码: 0 = 三个 unit 已 enable 且端到端验收通过; 1 = 任一步失败。
# 若出站池还没有 AnyIP local 路由, 会先自动调用 ./stage1-root.sh 做池探测与端到端验证。

set -u

here=$(cd "$(dirname "$0")" && pwd -P)   # -P: 解析符号链接(如 /home/x/workspace → /data/x/workspace)
LP="$here/light-proxy"
CFG="$here/config.toml"
UNIT_SRC="$here/units"
SVC="he-ipv6.service light-proxy-envsetup.service light-proxy.service"
ECHO_URL='http://api6.ipify.org/'
ECHO_URL_TLS='https://api6.ipify.org/'   # 走 CONNECT 隧道, 回显隧道内的源地址
PROBE_IP='2001:4860:4860::8888'
LEGACY_MODLOAD=/etc/modules-load.d/sit.conf

say() { printf '\n=== %s\n' "$*"; }
die() { printf '\n[FAIL] %s\n' "$*"; exit 1; }

[ "$(id -u)" -eq 0 ] || die "请用 sudo 运行: sudo $0"
[ -x "$LP" ] || die "$LP 不存在或不可执行
       (在项目根目录执行: CGO_ENABLED=0 go build -o temp/light-proxy ./cmd/light-proxy)"
[ -f "$CFG" ] || die "缺少 $CFG"
for u in he-ipv6 light-proxy-envsetup light-proxy; do
  [ -f "$UNIT_SRC/$u.service" ] || die "缺少 $UNIT_SRC/$u.service"
done

# ------------------------------------------------------------- 1. 运行用户
# 不新建用户: 直接用执行 sudo 的那个普通用户跑服务(可显式传用户名覆盖)。
say "确定服务运行用户"
runuser=${1:-}
[ -n "$runuser" ] || runuser=${SUDO_USER:-}
if [ -z "$runuser" ] || [ "$runuser" = root ]; then
  logname_user=$(logname 2>/dev/null || true)
  if [ -n "$logname_user" ] && [ "$logname_user" != root ]; then runuser=$logname_user; fi
fi
if [ -z "$runuser" ] || [ "$runuser" = root ]; then
  die "无法确定普通运行用户。请用 sudo 从目标用户执行, 或显式指定:
       sudo $0 <username>"
fi
id -u "$runuser" >/dev/null 2>&1 || die "用户 $runuser 不存在"
printf '  %s (uid %s, 组 %s)\n' "$runuser" "$(id -u "$runuser")" "$(id -gn "$runuser")"

# ------------------------------------------------------------- 2. 读配置
say "读取 $CFG"
# TOML 取值: [^"]* 到第一个收尾引号为止, 再吞掉行尾注释/空白 (配置里常带行尾注释)
pool=$(sed -n 's/^ipv6_pool *= *"\([^"]*\)".*/\1/p' "$CFG")
user=$(sed -n 's/^user *= *"\([^"]*\)".*/\1/p' "$CFG")
pass=$(sed -n 's/^pass *= *"\([^"]*\)".*/\1/p' "$CFG")
listen=$(sed -n 's/^listen *= *"\([^"]*\)".*/\1/p' "$CFG")
[ -n "$listen" ] || listen=':28888'
port=${listen##*:}
[ -n "$pool" ] || die "读不到 $CFG 里的 network.ipv6_pool"
case "$pool" in */64) ;; *) die "配置里的池不是 /64: $pool" ;; esac
[ -n "$user" ] && [ -n "$pass" ] || die "读不到 $CFG 里的 auth.user/auth.pass"
case "$port" in ''|*[!0-9]*) die "无法从 listen = \"$listen\" 解析端口" ;; esac
printf '  池 %s\n  监听 %s\n  凭据 %s / %s\n' "$pool" "$listen" "$user" "$pass"
# 服务以 $runuser 运行, 必须能读配置、能执行二进制
sudo -u "$runuser" test -r "$CFG" || die "$runuser 读不到 $CFG (改权限: chmod 0644 $CFG)"
sudo -u "$runuser" test -x "$LP"  || die "$runuser 无法执行 $LP"
echo "  运行用户可读配置、可执行二进制: 通过"

# ------------------------------------------------------------- 2b. 日志目录
# 配置里的 log.dir 相对工作目录(unit 的 WorkingDirectory=$here); unit 只能放行
# 一个 ReadWritePaths, 所以这里按配置解析出绝对路径, 既创建它又填进 unit。
say "准备日志目录"
# "dir"/"file" 出现但为空 = 显式关闭文件日志; 完全不出现 = 用内置默认值
dir_line=$(sed -n '/^dir *=/p' "$CFG")
logdir=$(sed -n 's/^dir *= *"\([^"]*\)".*/\1/p' "$CFG")
[ -n "$dir_line" ] || logdir=log
case "$logdir" in ""|/*) ;; *) logdir="$here/$logdir" ;; esac
[ -n "$logdir" ] || logdir="$here/log"          # ReadWritePaths 不能为空
file_line=$(sed -n '/^file *=/p' "$CFG")
logfile=$(sed -n 's/^file *= *"\([^"]*\)".*/\1/p' "$CFG")
[ -n "$file_line" ] || logfile=light-proxy.log
install -d -o "$runuser" -g "$(id -gn "$runuser")" -m 0755 "$logdir" || die "创建 $logdir 失败"
sudo -u "$runuser" test -w "$logdir" || die "$runuser 无法写 $logdir (改权限: chown $runuser $logdir)"
if [ -z "$logfile" ]; then
  echo "  $logdir 已就绪 (配置里 log.file 为空: 文件日志关闭, 只写 stdout)"
else
  echo "  $logdir (服务会追加写 $logdir/$logfile; unit 的 ReadWritePaths 指向它)"
fi

# ------------------------------------------------------------- 3. systemd unit
# 只装 unit 声明; ExecStart/配置路径全部指向 $here (temp/), 不复制程序文件。
say "安装 systemd unit (执行 $here/light-proxy, 运行用户 $runuser)"
for u in he-ipv6 light-proxy-envsetup light-proxy; do
  tmp=$(mktemp) || die "mktemp 失败"
  sed -e "s|__TEMP_DIR__|$here|g" -e "s|__RUN_USER__|$runuser|g" -e "s|__LOG_DIR__|$logdir|g" "$UNIT_SRC/$u.service" > "$tmp" \
    || { rm -f "$tmp"; die "生成 $u.service 失败"; }
  left=$(sed -n '/__TEMP_DIR__\|__RUN_USER__\|__LOG_DIR__/p' "$tmp")
  [ -z "$left" ] || { rm -f "$tmp"; die "$u.service 模板里有未替换的占位符: $left"; }
  install -m 0644 "$tmp" "/etc/systemd/system/$u.service" || { rm -f "$tmp"; die "安装 $u.service 失败"; }
  rm -f "$tmp"
  echo "  /etc/systemd/system/$u.service"
done
case "$(sed -n '/^User=/p' /etc/systemd/system/light-proxy.service)" in
  "User=$runuser") ;;
  *) die "light-proxy.service 的 User= 不是 $runuser" ;;
esac
case "$(sed -n '/^ReadWritePaths=/p' /etc/systemd/system/light-proxy.service)" in
  "ReadWritePaths=$logdir") ;;
  *) die "light-proxy.service 的 ReadWritePaths= 不是 $logdir" ;;
esac
case "$(sed -n '/^WorkingDirectory=/p' /etc/systemd/system/light-proxy.service)" in
  "WorkingDirectory=$here") ;;
  *) die "light-proxy.service 的 WorkingDirectory= 不是 $here" ;;
esac
systemctl daemon-reload || die "systemctl daemon-reload 失败"

say "清理旧设计留下的系统目录文件"
if [ -f "$LEGACY_MODLOAD" ] && [ "$(cat "$LEGACY_MODLOAD")" = sit ]; then
  rm -f "$LEGACY_MODLOAD" && echo "  已删除 $LEGACY_MODLOAD (sit 模块由 he-ipv6.service 自己 modprobe)"
else
  echo "  $LEGACY_MODLOAD 不存在或非本脚本所写, 未改动"
fi
for f in /usr/local/bin/light-proxy /etc/light-proxy.toml /etc/light-proxy.env; do
  if [ -e "$f" ]; then echo "  [注意] 遗留 $f (本设计不再使用, 可删: sudo rm -f $f)"; fi
done

# ------------------------------------------- 4. 隧道先起来, 并验证 IPv6 出口
say "启用并重启 he-ipv6.service (会先删掉手工建的隧道, 再按 unit 重建)"
systemctl enable he-ipv6.service >/dev/null 2>&1
systemctl restart he-ipv6.service || { systemctl --no-pager -l status he-ipv6.service | sed -n '1,20p'; die "he-ipv6.service 启动失败"; }
ip -6 addr show dev he-ipv6 | sed -n '/inet6/p'
ip -6 route show default | sed -n '/he-ipv6/p'
if ! ping -6 -c2 -W3 "$PROBE_IP" >/dev/null; then
  cat <<EOF

[FAIL] 隧道已建但 IPv6 出口不通 (ping -6 $PROBE_IP 无回包)。排查顺序:
  1) sudo tcpdump -ni eno2 'proto 41'   # 有出包无回包 → DC/上游过滤了 proto 41
  2) sudo nft list ruleset              # 本机防火墙是否 DROP 了 proto 41
  3) 门户 Tunnel Details 里该隧道是否 Up, Client IPv4 是否仍为 23.94.112.250
EOF
  exit 1
fi
echo "  IPv6 出口正常"

# ------------------------------------------- 5. 出站池: AnyIP local 路由
say "确认出站池 $pool 的 AnyIP local 路由"
case "$(ip -6 route show table local)" in
  *"local $pool"*) echo "  local $pool 已存在" ;;
  *)
    echo "  池 $pool 尚无 local 路由 → 先执行阶段 1 探测(会自动确定真正可用的前缀并回写 $CFG)"
    "$here/stage1-root.sh" || die "阶段 1 未通过; 按上面的提示处理后重跑本脚本"
    pool=$(sed -n 's/^ipv6_pool = "\(.*\)"/\1/p' "$CFG")
    echo "  探测结果: $pool"
    ;;
esac

say "doctor 实拨确认池 $pool"
"$LP" doctor -c "$CFG" || die "doctor 未通过。若 live dial 失败: 到门户抄 Routed /64 后执行
       sudo $here/stage1-root.sh <routed-prefix>/64
       再重跑本脚本"

# ------------------------------------------ 6. 启用 AnyIP 路由 unit 与代理 unit
say "启用并启动 light-proxy-envsetup.service 与 light-proxy.service"
if systemctl is-active --quiet light-proxy.service; then :; else
  inuse=$(ss -ltn 2>/dev/null | awk '{print $4}' | sed -n "/:$port\$/p" | sed -n '1p')
  [ -z "$inuse" ] || echo "  [注意] 端口 $port 已被占用 ($inuse); 若代理起不来请改 $CFG 的 listen"
fi
systemctl enable $SVC >/dev/null 2>&1
systemctl restart light-proxy-envsetup.service || { systemctl --no-pager -l status light-proxy-envsetup.service | sed -n '1,20p'; die "envsetup 失败"; }
systemctl restart light-proxy.service        || { systemctl --no-pager -l status light-proxy.service | sed -n '1,25p'; die "light-proxy 启动失败"; }
for _ in $(seq 1 40); do   # 等监听就绪(未鉴权请求返回 407, curl 视为成功)
  curl -s -o /dev/null -m1 "http://127.0.0.1:$port/" && break
  sleep 0.25
done
systemctl is-active --quiet light-proxy.service \
  || { systemctl --no-pager -l status light-proxy.service | sed -n '1,25p'; die "light-proxy 未保持 active"; }
if [ -n "$logfile" ]; then
  # 启动即写 "logging to file"/"listening" 两行, 足以证明 unit 放行的路径可写
  for _ in $(seq 1 20); do [ -s "$logdir/$logfile" ] && break; sleep 0.25; done
  [ -s "$logdir/$logfile" ] || die "服务已 active, 但 $logdir/$logfile 仍为空
       (检查 unit 的 ReadWritePaths=$logdir 与配置 log.dir/file; 服务日志: journalctl -u light-proxy -n 20)"
  echo "  请求日志已开始写: $logdir/$logfile"
fi

# ------------------------------------------------------------- 7. 端到端验收
say "端到端验收 (走 systemd 管理的服务, 127.0.0.1:$port)"
proxy="http://$user:$pass@127.0.0.1:$port"
code_noauth=$(curl -s -o /dev/null -w '%{http_code}' -m 5 -x "http://127.0.0.1:$port" "$ECHO_URL")
s1=$(curl -s -m 25 -x "$proxy" "$ECHO_URL")
s2=$(curl -s -m 25 -x "$proxy" "$ECHO_URL")
s3=$(curl -s -m 25 -x "$proxy" "$ECHO_URL")
printf '  无凭据请求 : HTTP %s (期望 407)\n' "$code_noauth"
printf '  三次实拨源地址:\n    %s\n    %s\n    %s\n' "$s1" "$s2" "$s3"

verdict=0
case "$code_noauth" in
  407) ;;
  *) echo "  [FAIL] 无凭据请求未返回 407"; verdict=1 ;;
esac
for s in "$s1" "$s2" "$s3"; do
  case "$(ip -6 route get "$s" 2>/dev/null)" in
    *"dev lo"*) ;;
    *) printf '  [FAIL] %s 不在 %s 的 AnyIP 路由内\n' "$s" "$pool"; verdict=1 ;;
  esac
done
[ "$s1" != "$s2" ] && [ "$s2" != "$s3" ] && [ "$s1" != "$s3" ] || { echo "  [FAIL] 三次源地址不唯一"; verdict=1; }
# 请求日志: 上面三次请求应各留一行, 且记录了轮换源地址
if [ -n "$logfile" ]; then
  req_count=$(sed -n '/msg=request/p' "$logdir/$logfile" | wc -l)
  last_req=$(sed -n '/msg=request/p' "$logdir/$logfile" | sed -n '$p')
  printf '  请求日志 %s 行: %s\n' "$req_count" "$(printf '%s' "$last_req" | cut -c1-160)"
  case "$last_req" in
    *status=200*"src=$s3"*) echo "  最后一次请求的 src 与回显地址一致: $s3" ;;
    *status=200*"src=${pool%%::*}:"*) echo "  最后一次请求已记录 src (在池内; 文本形式与回显略有差异)" ;;
    *) printf '  [FAIL] 请求日志里没有匹配最后一次请求的行 (期望 status=200 src=%s)\n' "$s3"; verdict=1 ;;
  esac
fi
# CONNECT 隧道: 必须用 -v 才看得到握手行 (-s 会把它连 curl 的错误信息一起吞掉)
conn_out=$(curl -sv -m 15 -x "$proxy" "$ECHO_URL_TLS" 2>&1); conn_rc=$?
t1=$(curl -s -m 25 -x "$proxy" "$ECHO_URL_TLS")
t2=$(curl -s -m 25 -x "$proxy" "$ECHO_URL_TLS")
case "$conn_out" in
  *'200 Connection Established'*) echo "  CONNECT 隧道 → 200 Connection Established" ;;
  *) printf '  [FAIL] CONNECT 隧道未建立 (curl exit=%s), curl 输出:\n' "$conn_rc"
     printf '%s\n' "$conn_out" | sed -n '/CONNECT/p;/HTTP\/1.1/p;/curl:/p' | sed -n '1,6p'
     verdict=1 ;;
esac
printf '  隧道内两次实拨源地址:\n    %s\n    %s\n' "$t1" "$t2"
for t in "$t1" "$t2"; do
  case "$(ip -6 route get "$t" 2>/dev/null)" in
    *"dev lo"*) ;;
    *) printf '  [FAIL] %s 不在 %s 的 AnyIP 路由内\n' "$t" "$pool"; verdict=1 ;;
  esac
done
[ "$t1" != "$t2" ] || { echo "  [FAIL] 两条 CONNECT 隧道的源地址相同"; verdict=1; }
# 隧道日志行在隧道关闭时写出, 稍等它落盘再报告
if [ -n "$logfile" ]; then
  for _ in $(seq 1 20); do
    case "$(sed -n '/msg=connect/p' "$logdir/$logfile" | sed -n '$p')" in
      *"src=$t2"*) break ;;
    esac
    sleep 0.25
  done
  last_conn=$(sed -n '/msg=connect/p' "$logdir/$logfile" | sed -n '$p')
  printf '  隧道日志: %s\n' "$(printf '%s' "$last_conn" | cut -c1-160)"
  case "$last_conn" in
    *status=200*"src=$t2"*) echo "  隧道源地址与回显一致: $t2" ;;
    *status=200*"src=${pool%%::*}:"*) echo "  隧道已记录 src (在池内)" ;;
    *) echo "  [注意] 未在日志里匹配到 src=$t2 (隧道行在隧道关闭时写出)" ;;
  esac
fi

# --------------------------------------------------------------- 8. 结果汇总
say "单元状态"
systemctl --no-pager --plain --full is-active $SVC | sed -n '1,5p'
systemctl --no-pager --plain list-unit-files he-ipv6.service light-proxy-envsetup.service light-proxy.service | sed -n '1,6p'
echo
printf '  二进制   : %s\n' "$LP"
printf '  配置     : %s\n' "$CFG"
printf '  出站池   : %s\n' "$pool"
printf '  监听     : %s\n' "$listen"
printf '  运行用户 : %s\n' "$runuser"
printf '  凭据     : %s / %s\n' "$user" "$pass"
if [ -n "$logfile" ]; then
  printf '  日志文件 : %s/%s          (每个请求一行: tail -f %s/%s)\n' "$logdir" "$logfile" "$logdir" "$logfile"
else
  printf '  日志文件 : 已关闭 (配置 log.file 为空), 只有 journalctl\n'
fi
printf '  日志     : journalctl -u light-proxy -f        (隧道: journalctl -u he-ipv6)\n'
printf '  改配置后 : sudo systemctl restart light-proxy   (配置/二进制都在 temp/, 无需重装)\n'
printf '  验证命令 : curl -x http://%s:%s@127.0.0.1:%s %s\n' "$user" "$pass" "$port" "$ECHO_URL"

if [ "$verdict" -ne 0 ]; then
  echo
  echo "[FAIL] 端到端验收未通过, 见上面的失败项。"
  exit 1
fi

cat <<EOF

[PASS] 三个 unit 已 enable, 开机自启 + 端到端验收通过。
       系统目录里只有 3 个 unit 声明文件; 二进制与配置仍在 $here。

重启验收清单 (sudo reboot 之后逐条执行):
  systemctl is-active he-ipv6 light-proxy-envsetup light-proxy     # 三个都应是 active
  ip -6 route show table local | sed -n "\|local $pool|p"          # 应有 local $pool dev lo
  ip -6 addr show dev he-ipv6 | sed -n '/inet6/p'                  # 应有 2001:470:7:34d::2/64
  curl -x http://$user:$pass@127.0.0.1:$port $ECHO_URL             # 跑两次, 源地址应不同
  curl -x http://$user:$pass@127.0.0.1:$port $ECHO_URL_TLS         # HTTPS 走 CONNECT 隧道, 同上
  tail -n 5 $logdir/$logfile                                       # 每个请求一行, 含 src= 轮换源地址
  systemctl kill -s HUP light-proxy.service                        # 轮转后重新打开日志文件

回滚 (程序文件在 temp/, 不需要删):
  sudo systemctl disable --now $SVC
  sudo rm -f /etc/systemd/system/he-ipv6.service /etc/systemd/system/light-proxy-envsetup.service /etc/systemd/system/light-proxy.service
  sudo systemctl daemon-reload
  sudo ip -6 route del local $pool dev lo ; sudo ip tunnel del he-ipv6
  sudo rm -rf $logdir                      # 日志(保留则下次启动继续追加)
EOF
