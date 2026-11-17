#!/bin/sh
# 下载服务端用到的模型，逐个校验大小和 sha256（v6 C6；清单 deploy/server/models.lock，说明 deploy/server/MODELS.md）。
#
#   scripts/fetch-models.sh DEST                  从本仓库的 Release 下载，按 models.lock 的路径放进 DEST
#                                                 （= Dockerfile 的 models 构建上下文；本机一般是 ~/.local/share/haro）
#   scripts/fetch-models.sh --from-upstream DEST  从原发布方下载（压缩包会解出要用的那几个文件）
#   scripts/fetch-models.sh --flat DEST           按 Release 里的文件名平铺放进 DEST（建 Release 上传时用）
#
# DEST 里已有、并且校验通过的文件直接跳过；下载的校验不过就报错退出（不会留下半截或不对的文件）。
# 环境变量 HARO_MODELS_BASE_URL：换掉本仓库 Release 的下载地址（测试用，可以是 file://）。
set -eu

usage() {
	sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}

from=release
layout=tree
dest=
while [ $# -gt 0 ]; do
	case $1 in
	--from-upstream) from=upstream ;;
	--flat) layout=flat ;;
	-h | --help) usage ;;
	-*)
		echo "不认识的选项：$1" >&2
		usage
		;;
	*)
		[ -z "$dest" ] || usage
		dest=$1
		;;
	esac
	shift
done
[ -n "$dest" ] || usage

root=$(cd "$(dirname "$0")/.." && pwd)
lock=$root/deploy/server/models.lock
release=$(awk '$1 == "release" { print $2; exit }' "$lock")
[ -n "$release" ] || {
	echo "$lock 里没有 release 那一行" >&2
	exit 1
}
base_url=${HARO_MODELS_BASE_URL:-https://github.com/bukahou/haro/releases/download/$release}

if command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
else # macOS
	sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi
size_of() { wc -c <"$1" | tr -d ' '; }
# matches FILE SHA256 SIZE：文件存在，大小和 sha256 都对上
matches() { [ -f "$1" ] && [ "$(size_of "$1")" = "$3" ] && [ "$(sha256_of "$1")" = "$2" ]; }
download() { curl -fsSL --retry 3 --retry-delay 2 -o "$2" "$1"; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT TERM

# 去掉注释、空行和 release 行，剩下每行一个文件
grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' -e '^release[[:space:]]' "$lock" >"$work/entries"
mkdir -p "$dest"
fetched=0
kept=0
while read -r sha256 size asset path url member; do
	if [ "$layout" = flat ]; then target=$dest/$asset; else target=$dest/$path; fi
	if matches "$target" "$sha256" "$size"; then
		kept=$((kept + 1))
		continue
	fi
	staged=$work/staged
	if [ "$from" = release ]; then
		echo "下载 $asset"
		download "$base_url/$asset" "$staged"
	elif [ "$member" = - ]; then
		echo "下载 $url"
		download "$url" "$staged"
	else # 压缩包：同一个只下载、解开一次
		unpacked=$work/archive-$(printf '%s' "$url" | cksum | cut -d' ' -f1)
		if [ ! -d "$unpacked" ]; then
			echo "下载 $url"
			download "$url" "$work/archive"
			mkdir "$unpacked"
			tar -xf "$work/archive" -C "$unpacked"
			rm -f "$work/archive"
		fi
		[ -f "$unpacked/$member" ] || {
			echo "压缩包里没有 ${member}（${url}）" >&2
			exit 1
		}
		cp "$unpacked/$member" "$staged"
	fi
	if ! matches "$staged" "$sha256" "$size"; then
		echo "校验不过：$asset —— 应为 $size 字节 / sha256 ${sha256}，实际 $(size_of "$staged") 字节 / $(sha256_of "$staged")" >&2
		exit 1
	fi
	mkdir -p "$(dirname "$target")"
	mv -f "$staged" "$target"
	fetched=$((fetched + 1))
done <"$work/entries"
echo "模型齐了（${release}）：新下载 $fetched 个，原有 $kept 个 → $dest"
