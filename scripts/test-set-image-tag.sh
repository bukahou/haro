#!/bin/sh
# scripts/set-image-tag.sh 的测试（CI 的 scripts 一步跑；本机也能跑）：
#   scripts/test-set-image-tag.sh
# 在临时目录里放几种写法的 kustomization.yaml，逐个确认：只换掉目标镜像那一行的 newTag（行尾注释、别的镜像、其他内容一个字节都不变），
# 找不到、找到不止一个、标签写法不对时报错退出、文件不动。
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
script=$root/scripts/set-image-tag.sh
image=bukahou/haro-server
fail() {
	echo "❌ set-image-tag.sh 测试失败：$*" >&2
	exit 1
}

# expect_change 说明：$work/input.yaml 改成 v2 之后应和 $work/want.yaml 一模一样
expect_change() {
	cp "$work/input.yaml" "$work/got.yaml"
	sh "$script" "$work/got.yaml" "$image" v2 >"$work/out" 2>&1 || fail "$1：应改成功：$(cat "$work/out")"
	cmp -s "$work/got.yaml" "$work/want.yaml" || fail "$1：改出来的不对：$(diff "$work/want.yaml" "$work/got.yaml" || true)"
}

# expect_refusal 说明 退出码 [标签]：应以这个退出码拒绝，文件一个字节都不变
expect_refusal() {
	what=$1 code=$2 tag=${3:-v2}
	cp "$work/input.yaml" "$work/got.yaml"
	status=0
	sh "$script" "$work/got.yaml" "$image" "$tag" >"$work/out" 2>&1 || status=$?
	[ "$status" -eq "$code" ] || fail "${what}：退出码是 ${status}，应为 ${code}：$(cat "$work/out")"
	cmp -s "$work/got.yaml" "$work/input.yaml" || fail "${what}：拒绝时不应改文件"
}

echo "== 块写法：只换目标镜像的 newTag，保留行尾注释、别的镜像和其他内容"
cat >"$work/input.yaml" <<'EOF'
# 部署清单
resources:
  - deployment.yaml
images:
  - name: example/other
    newTag: v1 # 别的镜像不动
  - name: bukahou/haro-server
    newTag: v1 # 发版流程改这一行
namespace: haro
EOF
sed 's/^    newTag: v1 # 发版流程改这一行$/    newTag: v2 # 发版流程改这一行/' "$work/input.yaml" >"$work/want.yaml"
cmp -s "$work/input.yaml" "$work/want.yaml" && fail "测试数据没准备对（期望的结果和输入一样）"
expect_change "块写法"

echo "== 名字带引号、列表不缩进"
cat >"$work/input.yaml" <<'EOF'
images:
- name: "bukahou/haro-server"
  newName: docker.io/bukahou/haro-server
  newTag: v1
EOF
sed 's/^  newTag: v1$/  newTag: v2/' "$work/input.yaml" >"$work/want.yaml"
expect_change "名字带引号"

echo "== 标签本来就是 v2：照样成功，文件不变"
cp "$work/want.yaml" "$work/input.yaml"
expect_change "标签没变"

echo "== 没有这个镜像：报错，文件不动"
cat >"$work/input.yaml" <<'EOF'
images:
  - name: example/other
    newTag: v1
EOF
expect_refusal "没有这个镜像" 1

echo "== 同一个镜像写了两次：报错，文件不动"
cat >"$work/input.yaml" <<'EOF'
images:
  - name: bukahou/haro-server
    newTag: v1
  - name: bukahou/haro-server
    newTag: v1
EOF
expect_refusal "镜像写了两次" 1

echo "== newTag 写在 name 前面（脚本不认这种写法）：报错，文件不动"
cat >"$work/input.yaml" <<'EOF'
images:
  - newTag: v1
    name: bukahou/haro-server
EOF
expect_refusal "newTag 在 name 前面" 1

echo "== 标签里有不允许的字符：报错，文件不动"
cat >"$work/input.yaml" <<'EOF'
images:
  - name: bukahou/haro-server
    newTag: v1
EOF
expect_refusal "标签里有空格" 2 "v2 # 注入"
echo "✅ set-image-tag.sh 测试通过"
