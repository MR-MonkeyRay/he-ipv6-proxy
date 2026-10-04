# he-ipv6-proxy

轻量级 HTTP 正向代理：监听 IPv4，并从指定 IPv6 `/64` 地址池中随机选择源地址出站。

## 特性

- 支持明文 HTTP 和 HTTPS `CONNECT` 隧道。
- 明文 HTTP 每个请求、`CONNECT` 每条隧道使用一个新 IPv6 地址。
- 默认 24 小时内不重复使用地址；记录仅保存在内存中。
- 支持 Basic 认证，不向目标站点发送代理凭据或客户端 IP。
- 仅使用 IPv6 出站，不回退 IPv4。
- 代理进程无需 root。

## 环境要求

- Linux
- 一个已路由的 IPv6 `/64`
- `iproute2`
- Go 1.24+ 和 `make`（仅源码构建需要）

## 快速开始

```sh
make build
cp config.example.toml config.toml
```

编辑 `config.toml`：

```toml
[network]
ipv6_pool = "2001:db8:abcd::/64"

[auth]
enabled = true
user = "alice"
pass = "请替换为强密码"

[doctor]
test_target = "[可达的IPv6地址]:443"
```

初始化路由、检查环境并启动：

```sh
sudo bin/he-ipv6-proxy env-setup -c config.toml
bin/he-ipv6-proxy doctor -c config.toml
bin/he-ipv6-proxy run -c config.toml
```

完整配置及默认值见 [`config.example.toml`](config.example.toml)。认证信息也可通过 `HE_IPV6_PROXY_AUTH_USER` 和 `HE_IPV6_PROXY_AUTH_PASS` 设置。

## 网络路由

轮换地址能否正常收包，取决于以下两条路由。

### 代理运行环境

在代理所在主机、虚拟机或容器内添加 AnyIP 路由：

```sh
sudo ip -6 route add local 2001:db8:abcd::/64 dev lo
```

`env-setup` 会自动、幂等地完成此操作。

### 宿主机或上游路由器

代理位于 LXC 或 KVM 客户机时，还需在宿主机配置回程路由：

```sh
sudo ip -6 route replace 2001:db8:abcd::/64 via fe80::1234 dev br0
```

其中 `fe80::1234` 是客户机的链路本地地址。不要将整个 `/64` 直接配置为 on-link。

KVM 还需满足：

- 客户机网卡接入 Linux bridge，不使用 NAT 或 macvtap。
- 宿主机启用 `net.ipv6.conf.all.forwarding=1`。
- 防火墙允许 IPv6 转发。

上游路由器也必须将该 `/64` 指向宿主机或客户机。

## systemd 部署

```sh
cp config.example.toml config.toml
# 编辑 config.toml
make build
sudo scripts/install-systemd.sh
```

安装脚本会创建无登录用户，并安装：

- `/usr/local/bin/he-ipv6-proxy`
- `/etc/he-ipv6-proxy.toml`
- `he-ipv6-proxy-envsetup.service`
- `he-ipv6-proxy.service`

```sh
systemctl status he-ipv6-proxy
journalctl -u he-ipv6-proxy -f
```

可将认证信息放入权限为 `0600` 的 `/etc/he-ipv6-proxy.env`。使用 Hurricane Electric 6in4 时，可参考 `deploy/systemd/he-ipv6.service.in`。

## 验证

连续请求应返回地址池内不同的 IPv6：

```sh
curl --proxy http://127.0.0.1:28888 --proxy-user 'alice:你的密码' http://api64.ipify.org
curl --proxy http://127.0.0.1:28888 --proxy-user 'alice:你的密码' http://api64.ipify.org
```

验证 HTTPS 隧道：

```sh
curl --proxy http://127.0.0.1:28888 --proxy-user 'alice:你的密码' https://api64.ipify.org
```

未提供或提供错误凭据时，代理返回 `407`。目标没有 AAAA 记录或 IPv6 不可达时，代理返回 `502`。

## 排错

| 现象 | 检查项 |
| --- | --- |
| 缺少 `AnyIP local route` | 运行 `sudo he-ipv6-proxy env-setup -c config.toml` |
| `live dial` 超时 | 检查宿主机回程路由、IPv6 转发、防火墙和上游路由 |
| `connection refused` | 网络已通，检查目标地址和端口 |
| HTTPS 返回 `502` | 检查目标 AAAA 记录、IPv6 连通性和代理日志 |

## 注意事项

- Basic 认证不会加密客户端到代理的连接。请限制访问来源，不要将开放代理暴露到公网。
- 明文 HTTP 按请求轮换；一条 `CONNECT` 隧道在整个生命周期内只使用一个源地址。
- 地址去重状态不会持久化，进程重启后清空。
- 请求日志包含目标主机、路径和查询参数，默认写入 stdout 和 `log/he-ipv6-proxy.log`。

## 开发

```sh
make check
make build
```
