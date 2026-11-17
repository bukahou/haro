#!/usr/bin/env bash
# 以 systemd 用户服务运行 haro 时的启动器（v3 第 4 步，4-2）。
# systemd 不经过登录 shell，读不到 shell 启动文件里加载的配置 ⇒ 这里先 source haro.env（和交互终端里一样，配置只有一份），
# 再在仓库根目录启动程序。凭证只在 haro.env 里，.service 文件不放任何凭证。
#
# 用法：run-with-env.sh bin/haro-brain
set -euo pipefail
cd "$(dirname "$0")/../.."
: "${HARO_ENV_FILE:?未设置 HARO_ENV_FILE（haro.env 的位置，在 .service 里设）}"
# shellcheck source=/dev/null
source "$HARO_ENV_FILE"
exec "$@"
