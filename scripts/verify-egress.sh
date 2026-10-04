#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  printf '%s\n' "请用 root 运行: sudo $0 [config] [binary]" >&2
  exit 1
fi

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd -P)
config=${1:-$repo/config.toml}
binary=${2:-$repo/bin/light-proxy}

[ -r "$config" ] || {
  printf '%s\n' "无法读取配置: $config" >&2
  exit 1
}
[ -x "$binary" ] || {
  printf '%s\n' "无法执行二进制: $binary；先运行 make build" >&2
  exit 1
}

"$binary" env-setup -c "$config"
"$binary" doctor -c "$config"
printf '%s\n' "出站环境检查通过。代理行为测试请运行: go test ./..."
