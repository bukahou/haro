#!/bin/sh
# haro 终端的安装脚本。用法、选项、做的事只写在下面的 usage 里（--help 打印它）：
# curl 管道装法（curl … | sh -s -- …）下 $0 不是这个文件，读不到这里的注释。
set -eu

repo=bukahou/haro
bin_dir=$HOME/.local/bin
binary=$bin_dir/haro
config_dir=$HOME/.config/haro
env_file=$config_dir/haro.env
unit_file=$HOME/.config/systemd/user/haro.service
launchd_label=com.github.bukahou.haro
plist_file=$HOME/Library/LaunchAgents/$launchd_label.plist
log_file=$HOME/Library/Logs/haro.log
marker='由 haro 安装脚本生成' # 服务文件里有这句才是本脚本装的：才会改动或删除

say() { printf '%s\n' "$*"; }
warn() { printf '⚠️  %s\n' "$*" >&2; }
die() {
	printf '❌ %s\n' "$*" >&2
	exit 1
}

# 拒装的原因各有一个退出码（scripts/test-install.sh 按退出码判断，不比对提示文字）；避开 shell 自己用的 1、2 和 126 以上
exit_need_server_url=10 # 第一次安装没给服务端地址
exit_checksum=11        # 下载的安装包和 SHA256SUMS 对不上
exit_no_platform=12     # 这个版本没有这台机器的平台

# refuse 退出码 说明：因为某个原因拒装
refuse() {
	code=$1
	shift
	printf '❌ %s\n' "$*" >&2
	exit "$code"
}

usage() {
	cat <<EOF
haro 终端的安装脚本（装在自己的用户目录里，不需要 root）。再跑一次 = 升级到最新。

  curl -fsSL https://github.com/$repo/releases/latest/download/install.sh | sh -s -- --server-url http://服务端地址:59130

选项：
  --server-url URL   服务端地址（第一次安装时必填；配置文件已经有了就不改它）
  --version TAG      装指定版本，例如 v0.5.0-1a2b3c4（默认最新）
  --no-service       只装程序和配置，不装开机自启
  --uninstall        卸载程序和开机自启（配置文件留着）
  -h, --help         打印这段说明

做的事：下载这台机器对应的终端程序（Linux x86_64 / arm64、macOS Apple 芯片）→ 按 SHA256SUMS 校验 →
  装到 ~/.local/bin/haro → 第一次装时写配置 ~/.config/haro/haro.env →
  装成开机自启并（重新）启动：Linux 是 systemd 用户服务 haro.service（跟着桌面会话启停），macOS 是 LaunchAgent。
不是这个脚本装的服务文件（例如开发时 make install-terminal-service 装的）不会被改动或删除。
环境变量 HARO_RELEASE_BASE_URL：换掉下载地址（测试用，可以是 file://；指向放着压缩包和 SHA256SUMS 的目录）。
EOF
}

server_url=
version=latest
with_service=true
action=install
while [ $# -gt 0 ]; do
	case $1 in
	--server-url)
		[ $# -ge 2 ] || die "--server-url 后面要跟地址"
		server_url=$2
		shift
		;;
	--server-url=*) server_url=${1#*=} ;;
	--version)
		[ $# -ge 2 ] || die "--version 后面要跟版本，例如 v0.5.0-1a2b3c4"
		version=$2
		shift
		;;
	--version=*) version=${1#*=} ;;
	--no-service) with_service=false ;;
	--uninstall) action=uninstall ;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "不认识的选项：$1（--help 看用法）" ;;
	esac
	shift
done

os=$(uname -s)
case $os in
Linux | Darwin) ;;
*) die "不支持这个系统：${os}（支持 Linux、macOS）" ;;
esac

# owned FILE：这个服务文件是本脚本装的（文件里有标记）
owned() { [ -f "$1" ] && grep -q "$marker" "$1"; }

# systemd 用户会话能用吗（SSH 登录、没有桌面会话时常常不能）
user_systemd_ready() { command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; }

uninstall() {
	if [ "$os" = Linux ] && [ -e "$unit_file" ]; then
		if owned "$unit_file"; then
			if user_systemd_ready; then systemctl --user disable --now haro.service || true; fi
			rm -f "$unit_file"
			if user_systemd_ready; then systemctl --user daemon-reload || true; fi
			say "已卸下开机自启：$unit_file"
		else
			warn "$unit_file 不是安装脚本装的（例如开发时 make install-terminal-service 装的），没有动它"
		fi
	fi
	if [ "$os" = Darwin ] && [ -e "$plist_file" ]; then
		if owned "$plist_file"; then
			launchctl bootout "gui/$(id -u)/$launchd_label" 2>/dev/null || true
			rm -f "$plist_file"
			say "已卸下开机自启：$plist_file"
		else
			warn "$plist_file 不是安装脚本装的，没有动它"
		fi
	fi
	if [ -e "$binary" ]; then
		if "$binary" -version 2>/dev/null | grep -q '^haro '; then
			rm -f "$binary"
			say "已删除程序：$binary"
		else
			warn "$binary 看起来不是 haro 终端，没有删"
		fi
	fi
	[ ! -e "$env_file" ] || say "配置文件留着：${env_file}（不要了就自己删）"
}

if [ "$action" = uninstall ]; then
	uninstall
	exit 0
fi

# ---- 下载、校验、装程序 ----
arch=$(uname -m)
if [ "$os" = Darwin ] && [ "$arch" = x86_64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ]; then
	arch=arm64 # Apple 芯片上用 Rosetta 跑的终端
fi
case $os/$arch in
Linux/x86_64 | Linux/amd64) platform=linux-amd64 ;;
Linux/aarch64 | Linux/arm64) platform=linux-arm64 ;;
Darwin/arm64) platform=darwin-arm64 ;;
*) die "不支持这个平台：$os ${arch}（支持 Linux x86_64 / arm64、macOS Apple 芯片）" ;;
esac
asset=haro-$platform.tar.gz

case $version in
latest) default_base=https://github.com/$repo/releases/latest/download ;;
v*) default_base=https://github.com/$repo/releases/download/$version ;;
*) die "版本要写成 v0.5.0-1a2b3c4 这样（GitHub Release 的标签）" ;;
esac
base_url=${HARO_RELEASE_BASE_URL:-$default_base}

if [ ! -e "$env_file" ]; then
	[ -n "$server_url" ] || refuse "$exit_need_server_url" "第一次安装要告诉终端服务端在哪：--server-url http://服务端地址:59130"
fi
if [ -n "$server_url" ]; then
	case $server_url in
	http://* | https://*) ;;
	*) die "服务端地址要以 http:// 或 https:// 开头，例如 http://192.0.2.10:59130" ;;
	esac
fi

command -v curl >/dev/null 2>&1 || die "需要 curl（先装上 curl 再运行）"
if command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
else
	sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT TERM

say "下载 ${asset}（${version}）"
curl -fsSL --retry 3 --retry-delay 2 -o "$work/SHA256SUMS" "$base_url/SHA256SUMS" ||
	die "下载不了 $base_url/SHA256SUMS（版本写对了吗？）"
# 发布的平台以这个版本的 SHA256SUMS 为准：不在里面就是这个版本没有这个平台的安装包
expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$work/SHA256SUMS")
[ -n "$expected" ] || refuse "$exit_no_platform" "这个版本没有 ${platform} 的安装包（发布了哪些平台见 $base_url/SHA256SUMS）"
curl -fsSL --retry 3 --retry-delay 2 -o "$work/$asset" "$base_url/$asset" ||
	die "下载不了 $base_url/$asset"
actual=$(sha256_of "$work/$asset")
[ "$actual" = "$expected" ] || refuse "$exit_checksum" "校验不过：$asset 的 sha256 是 ${actual}，SHA256SUMS 写的是 ${expected}（下载出错或文件被改过），没有安装"

mkdir -p "$work/unpacked" "$bin_dir"
tar -xzf "$work/$asset" -C "$work/unpacked"
[ -f "$work/unpacked/haro" ] || die "$asset 里没有 haro"
chmod 0755 "$work/unpacked/haro"
installed=$("$work/unpacked/haro" -version 2>/dev/null) || die "下载的程序在这台机器上跑不起来（平台不对？）"
mv -f "$work/unpacked/haro" "$binary" # 原子替换：正在运行的旧程序不受影响，重启服务后换成新的
say "已安装 $installed → $binary"

# ---- 配置文件：只在第一次安装时写；之后不改（手动改过的内容不会被覆盖）----
current=
if [ -e "$env_file" ]; then
	current=$(sed -n 's/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}HARO_SERVER_URL=["'\'']\{0,1\}\([^"'\'' ]*\).*/\2/p' "$env_file" | tail -n 1)
	if [ -n "$server_url" ] && [ "$server_url" != "$current" ]; then
		warn "配置文件已经有了，没有改它：服务端地址仍是「${current:-（没写）}」。要换成 $server_url 请编辑 $env_file"
	fi
else
	mkdir -p "$config_dir"
	cat >"$env_file" <<EOF
# haro 终端的配置（安装脚本第一次安装时写的；之后可以手动改，重新安装不会覆盖这个文件）。
# 每行 KEY="value"，启动时由 sh 读取。全部配置项和说明：https://github.com/$repo/blob/main/deploy/systemd/terminal.env.example
HARO_SERVER_URL="$server_url"

# 麦克风 / 音箱：空 = 跟系统默认（麦克风会跳过蓝牙的）。要指定就填设备 ID（麦克风的 ID 见启动日志 🎤 那一行的 id）；
# 填的设备不在时终端待机，日志里列出这台机器上现有的 ID
#HARO_AUDIO_SOURCE=""
#HARO_AUDIO_SINK=""
# 摄像头：空 = 自动找（macOS 上要装 ffmpeg）；none = 不用摄像头
#HARO_CAMERA_DEVICE=""
EOF
	say "已写配置：$env_file"
fi

# 服务端连得上吗（只提醒，不影响安装）
probe_url=${current:-$server_url} # 配置文件已有时，终端实际用的是文件里的地址
if [ -n "$probe_url" ] && ! curl -fsS --max-time 3 -o /dev/null "${probe_url%/}/livez" 2>/dev/null; then
	warn "现在连不上服务端 ${probe_url%/}/livez：确认地址对不对、服务端在不在运行（终端启动后会自己一直重连）"
fi
if [ "$os" = Darwin ] && ! command -v ffmpeg >/dev/null 2>&1; then
	warn "没找到 ffmpeg：macOS 上摄像头要用它（其他功能不受影响）。要用摄像头请先装 ffmpeg"
fi
if [ "$os" = Linux ] && ! command -v pactl >/dev/null 2>&1; then
	warn "没找到 pactl：认不出蓝牙麦克风和回录源（会照系统默认选麦克风）。建议先装 pulseaudio-utils"
fi
case ":$PATH:" in
*":$bin_dir:"*) ;;
*) say "提示：$bin_dir 不在 PATH 里，手动运行时请写全路径 $binary" ;;
esac

# ---- 开机自启 ----
install_systemd_unit() {
	if [ -e "$unit_file" ] && ! owned "$unit_file"; then
		warn "$unit_file 已存在而且不是安装脚本装的（例如开发时 make install-terminal-service 装的）：没有改它，开机自启没装"
		return 0
	fi
	if ! user_systemd_ready; then
		warn "这里用不了 systemd 用户服务（例如经 SSH 登录、没有桌面会话）：程序装好了，开机自启没装"
		say "  手动运行：sh -c 'set -a; . \"$env_file\"; exec \"$binary\"'"
		return 0
	fi
	mkdir -p "$(dirname "$unit_file")"
	cat >"$unit_file" <<EOF
# ${marker}（${installed}）。重新运行安装脚本会覆盖本文件；卸载：安装脚本加 --uninstall
# 依赖桌面会话里的 PipeWire（麦克风等设备的权限只发给登录中的桌面会话）⇒ 跟着桌面会话启停。
# 服务端重启、断线时终端不退出，自己重连；Restart 只作兜底。
[Unit]
Description=haro 终端
Documentation=https://github.com/$repo
After=graphical-session.target pipewire.service wireplumber.service
PartOf=graphical-session.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
Environment=HARO_ENV_FILE=%h/.config/haro/haro.env
ExecStart=/bin/sh -c 'set -a; . "\$\${HARO_ENV_FILE}"; exec "\$\$0"' %h/.local/bin/haro
Restart=on-failure
RestartSec=3

[Install]
WantedBy=graphical-session.target
EOF
	systemctl --user daemon-reload
	systemctl --user enable --quiet haro.service
	systemctl --user restart haro.service
	say "已装成 systemd 用户服务并启动：haro.service（日志：journalctl --user -u haro -f）"
}

install_launch_agent() {
	if [ -e "$plist_file" ] && ! owned "$plist_file"; then
		warn "$plist_file 已存在而且不是安装脚本装的：没有改它，开机自启没装"
		return 0
	fi
	mkdir -p "$(dirname "$plist_file")" "$(dirname "$log_file")"
	cat >"$plist_file" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ${marker}（${installed}）。重新运行安装脚本会覆盖本文件；卸载：安装脚本加 --uninstall -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$launchd_label</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>-c</string>
		<string>set -a; . "\$HOME/.config/haro/haro.env"; exec "\$HOME/.local/bin/haro"</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>$log_file</string>
	<key>StandardErrorPath</key>
	<string>$log_file</string>
</dict>
</plist>
EOF
	launchctl bootout "gui/$(id -u)/$launchd_label" 2>/dev/null || true
	launchctl bootstrap "gui/$(id -u)" "$plist_file"
	say "已装成 LaunchAgent 并启动：${launchd_label}（日志：tail -f ${log_file}）"
	say "第一次运行时 macOS 会问能不能用麦克风；没弹窗时到「系统设置 → 隐私与安全性 → 麦克风」里允许"
}

if [ "$with_service" = true ]; then
	case $os in
	Linux) install_systemd_unit ;;
	Darwin) install_launch_agent ;;
	esac
fi
say "完成。升级：重新运行同一条安装命令；卸载：加 --uninstall"
