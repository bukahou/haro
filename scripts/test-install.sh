#!/bin/sh
# 安装脚本的冒烟测试（CI 的 build 一步在三个平台上各跑一次；本机也能跑）：
#   scripts/test-install.sh 压缩包 期望的版本号
#   例：scripts/test-install.sh dist/haro-linux-amd64.tar.gz v0.5.0-1a2b3c4
# 在临时目录里假装一个 Release（压缩包 + SHA256SUMS）和一个空的家目录，用 install.sh 装一遍、再装一遍、卸一遍；
# 外加阳性对照：SHA256SUMS 不对时必须拒装。全程 --no-service（不碰这台机器的 systemd / launchd），不动真实的家目录。
set -eu
[ $# -eq 2 ] || {
	sed -n '2,7p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}
tarball=$1
want=$2
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
release=$work/release
home=$work/home
mkdir -p "$release" "$home"
cp "$tarball" "$release/"
asset=$(basename "$tarball")
if command -v sha256sum >/dev/null 2>&1; then sum=$(sha256sum "$release/$asset" | cut -d' ' -f1); else sum=$(shasum -a 256 "$release/$asset" | cut -d' ' -f1); fi
printf '%s  %s\n' "$sum" "$asset" >"$release/SHA256SUMS"

# ${1+"$@"}：没有参数时什么都不展开（老的 sh 在 set -u 下展开空的 "$@" 可能报错）
installer() { HOME=$home HARO_RELEASE_BASE_URL=file://$release sh "$root/scripts/install.sh" --no-service ${1+"$@"}; }
fail() {
	echo "❌ 安装脚本测试失败：$*" >&2
	exit 1
}

# 拒装原因的退出码只定义在安装脚本里（exit_原因=数字）：从那里读，按退出码判断为什么拒装，不比对提示文字
refusal_code() { sed -n "s/^exit_$1=\([0-9][0-9]*\).*/\1/p" "$root/scripts/install.sh"; }

# expect_refusal 原因 说明 [安装脚本的参数…]：安装脚本必须以这个原因的退出码拒装
expect_refusal() {
	reason=$1
	what=$2
	shift 2
	code=$(refusal_code "$reason")
	[ -n "$code" ] || fail "安装脚本里没有拒装原因 exit_${reason}"
	status=0
	installer ${1+"$@"} >"$work/out" 2>&1 || status=$?
	[ "$status" -eq "$code" ] || fail "${what}：退出码是 ${status}，应为 ${code}（exit_${reason}）：$(cat "$work/out")"
}

echo "== --help：curl 管道装法下（脚本从标准输入读，\$0 不是脚本文件）也打印用法"
HOME=$home sh -s -- --help <"$root/scripts/install.sh" >"$work/out" 2>&1 || fail "--help 应正常退出：$(cat "$work/out")"
grep -F -- '--server-url' "$work/out" >/dev/null || fail "--help 没打印用法：$(cat "$work/out")"

echo "== 第一次安装不给服务端地址：必须拒绝"
# 按退出码确认是因为没给地址才拒绝，而不是脚本自己出错（脚本出错时也是「没装上」）
expect_refusal need_server_url "没给 --server-url 时应因此拒装"

echo "== 第一次安装"
installer --server-url http://192.0.2.10:59130
"$home/.local/bin/haro" -version | grep -F "haro $want " >/dev/null || fail "装上的版本不是 ${want}：$("$home/.local/bin/haro" -version)"
grep -Fx 'HARO_SERVER_URL="http://192.0.2.10:59130"' "$home/.config/haro/haro.env" >/dev/null || fail "配置文件里没有服务端地址"

echo "== 再装一次（升级）：配置文件不变"
cp "$home/.config/haro/haro.env" "$work/env.before"
installer --server-url http://192.0.2.20:59130
cmp -s "$work/env.before" "$home/.config/haro/haro.env" || fail "重装时改了配置文件"

echo "== 阳性对照：SHA256SUMS 不对时必须拒装"
printf '%064d  %s\n' 0 "$asset" >"$release/SHA256SUMS"
expect_refusal checksum "校验和不对时应因此拒装"

echo "== 这个版本没有这台机器的平台（SHA256SUMS 里只有别的平台）：必须拒装，并说明是没有这个平台"
printf '%s  %s\n' "$sum" "haro-other-platform.tar.gz" >"$release/SHA256SUMS"
expect_refusal no_platform "这个版本没有这个平台时应因此拒装"

echo "== 卸载：程序删掉，配置文件留着"
installer --uninstall
[ ! -e "$home/.local/bin/haro" ] || fail "卸载后程序还在"
[ -f "$home/.config/haro/haro.env" ] || fail "卸载时删了配置文件"
echo "✅ 安装脚本测试通过（${asset}，${want}）"
