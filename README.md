# he-ipv6-proxy

轻量级 IPv6 源地址轮换 HTTP 正向代理:监听一个 IPv4 端口,用共享 Basic 凭据鉴权,对每个请求从配置的 IPv6 `/64` 中随机取一个**全新源地址**出站(24 小时内不重复)。

## 特性

- **源地址轮换**:每请求新建连接 + `crypto/rand` 抽取 IID,`IPV6_FREEBIND` 逐 socket 设置,绑定非本地源地址**无需 root**,代理进程自身不改任何 sysctl(宿主机侧可能需要开启 IPv6 转发,见[前置条件](#前置条件))。
- **纯透传**:不添加 `Via` / `X-Forwarded-*` / 默认 Go `User-Agent`,客户端的 `Proxy-Authorization` 不会到达上游。
- **明文 HTTP + CONNECT 隧道**:`http://` 绝对形式请求经 `httputil.ReverseProxy` 转发;`CONNECT` 建立后只做字节透传(TLS 端到端,代理不解析隧道内任何字节,也不做中间人)。目标无 AAAA 记录返回 `502`(纯 IPv6 出站,不回退 IPv4)。
- **单依赖** `BurntSushi/toml`,静态二进制;去重状态仅存内存,重启即清空(数据非关键)。
- **请求日志**:每个请求一行(客户端、方法、目标、状态码、字节数、**轮换源地址**、耗时),同时写 stdout 与配置文件里的本地文件(默认 `log/he-ipv6-proxy.log`);见[请求日志](#请求日志)。
- `doctor` 逐项探测前置条件并打印可直接执行的修复命令;`run` 启动前 fail-fast 自检。

## 构建

需要 Go 1.24 或更高版本。

```sh
make build
```

生成的静态二进制位于 `bin/he-ipv6-proxy`。

## 快速开始

```sh
cp config.example.toml config.toml              # 至少修改 network.ipv6_pool 与 auth.*
sudo bin/he-ipv6-proxy env-setup -c config.toml   # 应用客户机内 AnyIP 路由(幂等)
bin/he-ipv6-proxy doctor -c config.toml           # 全绿后再启动
bin/he-ipv6-proxy run -c config.toml
```

## 前置条件

需要两处路由,其中第二处**只能**在客户机/容器之外配置。

**1. 客户机/容器内 AnyIP 路由**(需 root 或 `CAP_NET_ADMIN`)。否则发往轮换源地址的回包不会被本地投递:

```sh
ip -6 route add local <pool>/64 dev lo    # env-setup 会幂等地执行
```

**2. 宿主机侧 nexthop 路由**(在 LXC 宿主机或 KVM hypervisor 上执行,客户机内无法完成):

```sh
ip -6 route add <pool>/64 via <客户机链路本地地址> dev <宿主机上的网桥或 veth>
```

必须**经由客户机链路本地地址**,不能直接把 `/64` 挂成 on-link:客户机只为自己的地址应答 NS,不会为 `/64` 内的任意地址做邻居发现。下一跳写成客户机的链路本地地址后,宿主机先解析出客户机 MAC,再按二层把包交给客户机,客户机用第 1 条 local 路由收下。

`<pool>/64` 还需要上游路由器指向宿主机;若上游路由器与客户机同二层,也可直接在路由器上 `ip -6 route add <pool>/64 via <客户机链路本地地址> dev <上行口>`,此时宿主机只做二层转发。

IPv6 没有 `rp_filter`,无需关闭任何东西。

## 在 LXC 容器中运行

```sh
# 容器内
sudo he-ipv6-proxy env-setup -c /etc/he-ipv6-proxy.toml
ip -6 addr show scope link                  # 取链路本地地址,交给宿主机
he-ipv6-proxy doctor -c /etc/he-ipv6-proxy.toml

# 宿主机(LXC host)
ip -6 route add <pool>/64 via <容器链路本地地址> dev <host-veth>
```

LXC 自身的 `lxc.net.*.ipv6.route` 装的是 **on-link** 路由,对 `/64` 内任意地址不生效。用 `lxc.net.*.hwaddr` 固定 MAC,链路本地地址才稳定。

容器重启后第 1 条 `local` 路由消失,用[以 systemd 服务运行](#以-systemd-服务运行)里的 unit 恢复。

## 在 KVM 客户机中运行

客户机是一台完整虚拟机:**客户机内与 LXC 完全相同**(第 1 条路由 + `env-setup`),差别全在 hypervisor 侧。

### 1. 客户机网卡必须接在网桥上

libvirt 的这几种接法不能用于轮换:

| 接法 | 为什么不行 |
| --- | --- |
| `default`(NAT)网络 | 默认网络只有 IPv4 子网,客户机没有 IPv6 出口;显式开 IPv6 NAT(`<nat ipv6='yes'/>`)则源地址被改写成宿主机地址,轮换失去意义 |
| macvtap(`<interface type='direct'>`) | 宿主机与客户机之间无法直接通信,宿主机侧既加不了 nexthop 路由,也解析不到客户机链路本地地址 |
| `<forward mode='route'/>` | libvirt 只为该网络**声明的子网**装路由,`/64` 内任意地址仍要手动加路由(与 LXC 的 on-link 问题同源) |

用网桥:

```sh
# 宿主机:br0 已桥接上行网卡
virsh attach-interface <domain> --type bridge --source br0 --model virtio --config
```

### 2. hypervisor 必须转发 IPv6

```sh
sysctl -w net.ipv6.conf.all.forwarding=1
echo 'net.ipv6.conf.all.forwarding=1' > /etc/sysctl.d/99-he-ipv6-proxy.conf   # 持久化
```

转发关闭时,即使路由正确,`doctor` 的实拨探测也是 `i/o timeout`(与缺路由症状相同)。另外开启转发会把 `accept_ra` 从默认的 1 降为 0;hypervisor 上行靠 SLAAC 取地址时需 `net.ipv6.conf.<上行口>.accept_ra=2`。

### 3. 固定客户机的链路本地地址

宿主机路由的下一跳是客户机链路本地地址,它一变路由就废。二选一:

- 固定 MAC:`virsh edit <domain>`,在 `<interface>` 内写死 `<mac address='52:54:00:xx:xx:xx'/>`(默认 EUI-64 链路本地地址由 MAC 推导)。
- 或直接在客户机配静态链路本地地址(NetworkManager / systemd-networkd / netplan 均可),与 MAC 生成模式无关,推荐这种。

### 4. 宿主机加路由并自查

```sh
ip -6 route add <pool>/64 via <客户机链路本地地址> dev br0
ip -6 route get <pool 内任意地址>     # 应显示 via fe80::... dev br0
ip -6 neigh show dev br0             # 出现客户机链路本地地址 = 二层可达
```

宿主机若启用 firewalld/nftables,还要放行到 `br0` 的转发,否则静默丢包(症状同缺路由)。

### 5. 持久化宿主机路由

```ini
# /etc/systemd/system/he-ipv6-proxy-pool-route.service
[Unit]
Description=Route the he-ipv6-proxy pool /64 to the guest
After=network-online.target libvirtd.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'ip -6 route replace 2001:db8:abcd::/64 via fe80::1:2 dev br0'
ExecStop=/bin/sh -c 'ip -6 route del 2001:db8:abcd::/64 via fe80::1:2 dev br0'

[Install]
WantedBy=multi-user.target
```

客户机内的 `local` 路由见下节。

## 以 systemd 服务运行

仓库在 `deploy/systemd/` 提供通用 unit，在 `scripts/install-systemd.sh` 提供安装脚本。安装目标固定，避免 systemd 依赖 Git 工作树：

| 内容 | 安装路径 |
| --- | --- |
| 二进制 | `/usr/local/bin/he-ipv6-proxy` |
| 配置 | `/etc/he-ipv6-proxy.toml` |
| 可选凭据环境文件 | `/etc/he-ipv6-proxy.env` |
| 工作及日志目录 | `/var/lib/he-ipv6-proxy` |

```sh
cp config.example.toml config.toml
# 编辑 config.toml；如需把凭据移出配置，可在安装后创建 /etc/he-ipv6-proxy.env
make build
sudo scripts/install-systemd.sh
```

安装脚本创建无登录用户 `he-ipv6-proxy`，安装并启动 `he-ipv6-proxy-envsetup.service` 与 `he-ipv6-proxy.service`。代理本身无特权；只有一次性的 `env-setup` unit 以 root 身份恢复 AnyIP local 路由。

凭据环境文件示例（权限应为 `0600`）：

```sh
HE_IPV6_PROXY_AUTH_USER=alice
HE_IPV6_PROXY_AUTH_PASS=s3cret
```

使用 Hurricane Electric 6in4 时，先复制并填写 `deploy/systemd/he-ipv6.service.in` 的三个占位符，再将结果安装为 `/etc/systemd/system/he-ipv6.service`。该模板不包含机器地址；应用 unit 也不强制依赖特定隧道实现。

`ProtectSystem=strict` 下日志目录必须预先存在且归 `he-ipv6-proxy` 所有；安装脚本会创建 `/var/lib/he-ipv6-proxy/log`。

部署前也可运行 `sudo scripts/verify-egress.sh [config] [binary]`，它只执行 `env-setup` 和 `doctor`，不保存本机配置或凭据。

也可以不用 unit，直接声明式配置第 1 条 `local` 路由：

```ini
# 客户机 /etc/systemd/network/10-anyip.network
[Match]
Name=eth0

[Route]
Destination=2001:db8:abcd::/64
Type=local
```

netplan:`ethernets.<id>.routes: [{to: "2001:db8:abcd::/64", type: local}]`。

## 子命令

| 命令 | 作用 |
| --- | --- |
| `run` | 启动服务(默认命令);`runtime.selfcheck=true` 时先做前置自检 |
| `doctor` | 探测全部前置条件,逐项打印 `[ok]/[FAIL]/[skip]` 与修复命令 |
| `env-setup` | 幂等应用客户机内 AnyIP 路由,需 root |

`-c, --config <path>` 指定配置文件,默认 `config.toml`。

## 配置

见 `config.example.toml`,可省略任意键(取默认值)。

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `server.listen` | `:28888` | 监听地址 |
| `server.read_header_timeout` | `30s` | 客户端请求头读取超时 |
| `network.ipv6_pool` | 必填 | 必须是**已掩码**的 IPv6 `/64` |
| `network.dial_timeout` | `10s` | 单次上游连接超时 |
| `network.idle_timeout` | `60s` | 客户端 keep-alive 连接的空闲超时;已建立的 `CONNECT` 隧道不受其约束 |
| `network.response_header_timeout` | `30s` | 上游响应头超时 |
| `auth.enabled` | `true` | 置 `false` 即开放代理,仅限可信网络 |
| `auth.user` / `auth.pass` | — | 单一共享凭据,`enabled=true` 时必填 |
| `dedup.ttl` | `24h` | 源地址复用窗口 |
| `dedup.sweep_interval` | `5m` | 过期条目清理间隔 |
| `log.level` | `info` | `info`/`debug`;`debug` 额外打印每个连接的源地址 |
| `log.dir` | `log` | 日志目录,相对工作目录;置空即关闭文件日志 |
| `log.file` | `he-ipv6-proxy.log` | 日志文件名(只能是文件名,目录用 `log.dir`);置空即关闭文件日志 |
| `runtime.selfcheck` | `true` | `run` 启动前做前置自检 |
| `doctor.test_target` | 空 | 可选 IPv6 TCP 目标,用于 `doctor` 实拨验证回程;**IPv6 要带方括号**,如 `"[2001:db8::1]:80"` |

密钥可用 `HE_IPV6_PROXY_AUTH_USER` / `HE_IPV6_PROXY_AUTH_PASS` 覆盖(非空时生效),便于把凭据放在环境里。

## 验证

```sh
# 两次请求应返回不同且在 /64 内的源地址;回显头中无 Via/X-Forwarded-*/Proxy-Authorization
curl -x http://alice:s3cret@<ipv4>:28888 http://<echo>/ ; curl -x http://alice:s3cret@<ipv4>:28888 http://<echo>/
curl -x http://<ipv4>:28888 http://<echo>/                 # 407

# CONNECT 隧道:每条隧道一个全新源地址(HTTPS 走这条路径)
curl -sv -x http://alice:s3cret@<ipv4>:28888 https://<echo>/ 2>&1 | grep 'Connection Established'
curl -s -o /dev/null -w '%{http_code}\n' -x http://alice:s3cret@<ipv4>:28888 https://api.openai.com/v1/models  # 401 = 隧道通,仅缺 key
```

`curl -v` 里应看到 `CONNECT <host>:443` 后紧跟 `HTTP/1.1 200 Connection Established`;隧道建立后 `%{http_code}` 是目标站点的状态码(`000` 说明隧道没建立,退出码 56)。

`doctor.test_target` 指向可达的 IPv6 `host:port` 后,`he-ipv6-proxy doctor` 的实拨探测可一次性验证绑定、客户机内路由、宿主机回程路由。

宿主机侧自查:

```sh
ip -6 route get <pool 内任意地址>   # via fe80::... dev br0
ip -6 neigh show dev br0            # 客户机链路本地地址
```

项目检查：`make check`（运行 `go test ./...` 和 `go vet ./...`）。

请求日志(每个请求一行,`src=` 就是本次的轮换源地址):

```sh
tail -n 5 log/he-ipv6-proxy.log
sed -n '/msg=request/p' log/he-ipv6-proxy.log | sed -n '$p'
```

## 排错

| 症状 | 原因 |
| --- | --- |
| `doctor` 的 `live dial` 报 `i/o timeout`,而 `AnyIP local route` 是 `[ok]` | 宿主机缺 nexthop 路由、hypervisor 未开 IPv6 转发、或防火墙丢弃转发;三者症状相同,按序检查 |
| `live dial` 报 `connect: connection refused` | 路径已通,目标端口没监听(目标写错) |
| 客户机内 `ip -6 route get <pool 地址>` 不是 `local` | 客户机缺第 1 条路由,或重启后没恢复 |
| 宿主机 `ip -6 neigh show dev br0` 没有客户机链路本地地址 | 客户机未运行 / 网卡不在该网桥 / 链路本地地址变了 |
| `curl -x <proxy> https://<host>/` 退出码 56、`%{http_code}` = `000` | 隧道没建立:看 `-v` 里代理回的是 `502`(目标无 AAAA 或连不上)还是 `200 Connection Established` |

## 退出码

| 码 | 含义 |
| --- | --- |
| 0 | 正常 |
| 1 | 运行时失败(如端口占用、`doctor` 探测失败) |
| 2 | 配置错误(含日志目录/文件无法创建或打开) |
| 3 | 前置自检失败 |

## 请求日志

每个请求一行(`slog` text),**同时**写 stdout 与 `<log.dir>/<log.file>`(默认 `log/he-ipv6-proxy.log`,相对工作目录):

```
time=2026-09-18T15:58:10Z level=INFO msg=request client=127.0.0.1:10206 method=GET target=api6.ipify.org path=/ status=200 bytes=34 src=2001:db8:abcd:0:b343:fa75:f465:428d dur=262.8ms
time=2026-09-18T15:58:10Z level=INFO msg=connect client=127.0.0.1:10208 target=api6.ipify.org:443 status=200 src=2001:db8:abcd:0:ba38:3865:c4f9:8cc1 sent=720 received=5085 dur=487.8ms
```

| 字段 | 含义 |
| --- | --- |
| `msg` | `request` = 明文 HTTP 请求;`connect` = CONNECT 隧道 |
| `client` | 客户端地址(`CONNECT` 行写的是发起隧道的客户端) |
| `method` / `target` / `path` | 请求方法与目标主机;`path` 是 path+query,**不含**凭据 |
| `status` | 回给客户端的状态码;`0` = 还没写出响应就断了(隧道握手阶段) |
| `bytes` | 明文 HTTP 的响应体字节数 |
| `src` | 本次请求实际绑定的**轮换源地址**(无端口);`""` = 没拨号(407/400 等) |
| `sent` / `received` | 隧道的两个方向各转发多少字节(隧道关闭时才写出) |
| `dur` | 从进来到写出这一行(隧道行 = 隧道存活时间) |

- 失败也有行:407(未鉴权)、400(非法目标)、502(上游连不上)都会留一行;上游失败原因另有一行 `msg="upstream failure"`。
- 隧道行在**隧道关闭时**才写出,长连接(WebSocket、连接池复用)期间不会提前落盘。
- 轮转:日志文件无限增长,自己配 logrotate(或 `mv` 后发 HUP,进程会重新打开文件,无需重启):
  ```sh
  systemctl kill -s HUP he-ipv6-proxy.service     # 重新打开 log/he-ipv6-proxy.log
  ```
  未配置文件日志时 SIGHUP 保持默认行为(终止进程),不会被吞掉。
- 日志目录不可创建/不可写时启动即失败并返回退出码 2(与配置错误同类,unit 的 `RestartPreventExitStatus=2 3` 不会热循环)。
- 文件日志只作用于 `run`;`doctor` / `env-setup` 是打印报告的一次性命令,只写 stdout。

## 已知取舍

- 上游请求会携带 `Connection: close`:这是每请求新建连接(即轮换源地址)的结构性要求,不携带代理身份与客户端 IP。若要连一个额外头都不加,就必须复用连接,而连接复用与逐请求轮换互斥。
- 去重表不持久化,重启后 24 小时窗口重置;`/64` 内碰撞概率约 $2^{-64}$,可忽略。
- 无读写超时(`ReadTimeout`/`WriteTimeout` = 0),以便流式传输大响应;上游连接与响应头分别由 `dial_timeout`、`response_header_timeout` 约束。
- `CONNECT` 隧道建立后 `hijack` 连接,两端都没有读写超时(同上一条取舍):某一方向结束后另一方向最多再等 30s 排水,然后两端一起关闭;两端都不结束的隧道会一直保留(与任何裸 TCP 代理相同,靠两端或内核 keepalive 收尾)。
- 隧道内无法逐请求轮换源地址:一条 TLS 连接(及其上的 keep-alive / 多路复用)就是一条隧道 = 一个源地址。要逐请求换源,客户端必须每次新建连接(如禁用连接池)。
- 宿主机侧路由与客户机内 `local` 路由都不由本程序管理(客户机内可用 `env-setup` 恢复),重启后需要重建。
- 日志文件不做轮转,由外部 logrotate 负责(改名后发 HUP,进程重新打开);请求日志按行追加,量大会长得快,`log.file = ""` 可退回只写 stdout 交给 journald。
- 请求日志记录了目标主机与 path+query(用于排障),不含 `Proxy-Authorization`;敏感查询串会落进日志文件,权限按 `0644` 创建(需要更严可自行收紧目录权限)。
