#!/bin/sh
# 把 kustomization.yaml 里某个镜像的 newTag 改成新标签（发版流程 release.yml 的 deploy 一步用）。
#   scripts/set-image-tag.sh FILE IMAGE TAG
#   例：scripts/set-image-tag.sh kustomization.yaml bukahou/haro-server v0.5.0-1a2b3c4
# 只改 images 里 name 等于 IMAGE 的那一项的 newTag，保留行尾注释和文件里其他所有内容
# （不用 kustomize edit set image：它会重写整个文件，注释全丢；yq 也会按自己的格式重排缩进）。找不到这一项、
# 或者改出来的不止一行，就报错退出，文件不动。按 YAML 结构核对改对没有由调用方用 yq 做（release.yml 的 deploy）。
# 测试：scripts/test-set-image-tag.sh（CI 的 scripts 一步跑）
set -eu
[ $# -eq 3 ] || {
	sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
}
file=$1 image=$2 tag=$3
case $tag in
*[!A-Za-z0-9._-]* | "") echo "标签只能有字母、数字和 . _ -：$tag" >&2; exit 2 ;;
esac
updated=$(mktemp)
trap 'rm -f "$updated"' EXIT
awk -v image="$image" -v tag="$tag" '
	# 进入「- name: IMAGE」这一项；碰到下一项（下一个「- name:」或顶格的键）就离开；注释行不算
	match($0, /^[[:space:]]*-[[:space:]]*name:[[:space:]]*/) {
		name = substr($0, RLENGTH + 1)
		sub(/[[:space:]]+#.*$/, "", name)
		sub(/[[:space:]]+$/, "", name)
		gsub(/["\047]/, "", name)
		in_item = (name == image)
		print
		next
	}
	in_item && match($0, /^[[:space:]]*newTag:[[:space:]]*/) {
		prefix = substr($0, 1, RLENGTH)
		rest = substr($0, RLENGTH + 1)
		comment = ""
		if (match(rest, /[[:space:]]+#.*$/)) comment = substr(rest, RSTART)
		print prefix tag comment
		changed++
		in_item = 0
		next
	}
	in_item && /^[^[:space:]#-]/ { in_item = 0 }
	{ print }
	END { if (changed != 1) exit 3 }
' "$file" >"$updated" || {
	echo "$file 里没找到镜像 $image 的 newTag（或找到不止一个），没有改动" >&2
	exit 1
}
# 独立于上面的 awk 再核对一次：和原文件比，最多只有一行不同（标签本来就是 TAG 时一行都不变）
removed=$(diff "$file" "$updated" | grep -c '^<' || true)
added=$(diff "$file" "$updated" | grep -c '^>' || true)
if [ "$removed" -gt 1 ] || [ "$added" -ne "$removed" ]; then
	echo "${file}：改出来的和原文件差了 ${removed} 行删、${added} 行加，应只换一行 newTag，没有改动" >&2
	exit 1
fi
cat "$updated" >"$file" # 写回原文件（保留文件本身的权限、软链接）
