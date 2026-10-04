#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf '%s\n' "请用 root 运行: sudo $0" >&2
  exit 1
fi

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
binary=${BINARY:-$repo/bin/he-ipv6-proxy}
config=${CONFIG:-$repo/config.toml}
if [ -z "${CONFIG+x}" ] && [ ! -r "$config" ] && [ -r /etc/light-proxy.toml ]; then
  config=/etc/light-proxy.toml
fi

[ -x "$binary" ] || {
  printf '%s\n' "缺少可执行文件 $binary；先运行: make build" >&2
  exit 1
}
[ -r "$config" ] || {
  printf '%s\n' "缺少配置 $config；先复制并编辑 config.example.toml" >&2
  exit 1
}

if ! id he-ipv6-proxy >/dev/null 2>&1; then
  useradd --system --home-dir /var/lib/he-ipv6-proxy --shell /usr/sbin/nologin he-ipv6-proxy
fi

# Clean cutover from the former project name. Stop the old instance before the
# new one binds the same configured port.
systemctl disable --now light-proxy.service light-proxy-envsetup.service 2>/dev/null || true

install -m 0755 "$binary" /usr/local/bin/he-ipv6-proxy
if [ "$config" = /etc/he-ipv6-proxy.toml ]; then
  chown root:he-ipv6-proxy "$config"
  chmod 0640 "$config"
else
  install -o root -g he-ipv6-proxy -m 0640 "$config" /etc/he-ipv6-proxy.toml
fi
if [ -f /etc/light-proxy.env ] && [ ! -e /etc/he-ipv6-proxy.env ]; then
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' EXIT HUP INT TERM
  sed -e 's/^LIGHTPROXY_AUTH_USER=/HE_IPV6_PROXY_AUTH_USER=/' \
      -e 's/^LIGHTPROXY_AUTH_PASS=/HE_IPV6_PROXY_AUTH_PASS=/' \
      /etc/light-proxy.env > "$tmp"
  install -o root -g root -m 0600 "$tmp" /etc/he-ipv6-proxy.env
  rm -f "$tmp"
  trap - EXIT HUP INT TERM
fi
install -d -o he-ipv6-proxy -g he-ipv6-proxy -m 0750 /var/lib/he-ipv6-proxy/log
install -m 0644 "$repo/deploy/systemd/he-ipv6-proxy-envsetup.service" /etc/systemd/system/he-ipv6-proxy-envsetup.service
install -m 0644 "$repo/deploy/systemd/he-ipv6-proxy.service" /etc/systemd/system/he-ipv6-proxy.service

systemctl daemon-reload
systemctl enable --now he-ipv6-proxy-envsetup.service he-ipv6-proxy.service

rm -f /etc/systemd/system/light-proxy.service \
      /etc/systemd/system/light-proxy-envsetup.service \
      /usr/local/bin/light-proxy \
      /etc/light-proxy.toml \
      /etc/light-proxy.env
systemctl daemon-reload
printf '%s\n' "he-ipv6-proxy 已安装；状态: systemctl status he-ipv6-proxy"
