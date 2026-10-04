#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf '%s\n' "请用 root 运行: sudo $0" >&2
  exit 1
fi

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
binary=${BINARY:-$repo/bin/light-proxy}
config=${CONFIG:-$repo/config.toml}

[ -x "$binary" ] || {
  printf '%s\n' "缺少可执行文件 $binary；先运行: make build" >&2
  exit 1
}
[ -r "$config" ] || {
  printf '%s\n' "缺少配置 $config；先复制并编辑 config.example.toml" >&2
  exit 1
}

if ! id light-proxy >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/light-proxy --shell /usr/sbin/nologin light-proxy
fi
install -m 0755 "$binary" /usr/local/bin/light-proxy
install -o root -g light-proxy -m 0640 "$config" /etc/light-proxy.toml
install -d -o light-proxy -g light-proxy -m 0750 /var/lib/light-proxy/log
install -m 0644 "$repo/deploy/systemd/light-proxy-envsetup.service" /etc/systemd/system/light-proxy-envsetup.service
install -m 0644 "$repo/deploy/systemd/light-proxy.service" /etc/systemd/system/light-proxy.service

systemctl daemon-reload
systemctl enable --now light-proxy-envsetup.service light-proxy.service
printf '%s\n' "light-proxy 已安装；状态: systemctl status light-proxy"
