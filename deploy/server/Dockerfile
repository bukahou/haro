# syntax=docker/dockerfile:1
# haro-server 镜像：服务端（含记事本等内置工具）+ haro-asr（cgo，sherpa-onnx）。同一个镜像跑同一个 Pod 里的两个容器：
# 默认入口是服务端；haro-asr 的容器把入口换成 /opt/haro/bin/haro-asr。两者经本机地址通信（HARO_ASR_LISTEN / HARO_ASR_URL）。
#
# 构建（在仓库根目录；模型不在仓库里，用命名构建上下文 models 传进来，目录结构见 deploy/server/models.lock）：
#   发版：合并到 main 后由 GitHub Actions 构建并推送（.github/workflows/release.yml；模型从本仓库的 Release models-v1 下载并校验）
#   本机试：make image   # = docker buildx build -f deploy/server/Dockerfile --build-arg VERSION=… --build-context models=$HOME/.local/share/haro .
#
# 镜像里有什么 / 没什么（v4 4-1 b：公开仓库）：
#   ✅ 两个程序、识别 / 唤醒 / 声纹模型（都是公开模型；来源与许可证见 /opt/haro/models/MODELS.md）、唤醒词文件
#   ⛔ 性格档案、声音档案（由部署方的 ConfigMap 挂到 /etc/haro）、任何凭证（K8s Secret）、
#      声纹 / 记事 / 对话（数据卷 /data）
#
# 只做 x86（v4 D6）。
#
# 基础镜像钉在 digest 上（v6）：同一个提交重建时基础镜像不变；官方镜像重建后，Dependabot 每周开 PR 到 dev 改 digest
# （.github/dependabot.yml）。系统包的安全补丁在构建时打（apt-get upgrade）：补丁那一层按构建当时的 Debian 安全源，
# CI 有缓存时沿用上一次的，基础镜像 digest 一变就重新打。

FROM golang:1.27-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 版本号（v6）：= 镜像标签 = GitHub Release 标签，例如 v0.5.0-1a2b3c4
ARG VERSION=dev
# haro-server 纯 Go；haro-asr 要 cgo（sherpa-onnx 的原生库随 Go 模块下载，运行时用 LD_LIBRARY_PATH 找）
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X github.com/bukahou/haro/internal/buildinfo.Version=${VERSION}" -o /out/bin/haro-server ./cmd/haro-server \
 && go build -trimpath -ldflags "-X github.com/bukahou/haro/internal/buildinfo.Version=${VERSION}" -o /out/bin/haro-asr ./cmd/haro-asr \
 && mkdir -p /out/lib \
 && cp "$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux/v2)"/lib/x86_64-unknown-linux-gnu/*.so /out/lib/

FROM debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y \
 && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin haro \
 && mkdir -p /data /etc/haro && chown haro:haro /data
COPY --from=build /out/bin/ /opt/haro/bin/
COPY --from=build /out/lib/ /opt/haro/lib/
COPY LICENSE /opt/haro/LICENSE
COPY assets/wake-words.txt /opt/haro/assets/wake-words.txt
COPY deploy/server/MODELS.md deploy/server/MODEL-LICENSES.txt /opt/haro/models/
# 模型（只拷用到的文件，和 deploy/server/models.lock 一一对应：SenseVoice 用 int8 版；唤醒词用 chunk-16 的那一组）。
# 声纹模型保留原文件名：声纹记录里存的是模型文件名，换名字旧声纹就对不上了
COPY --from=models asr/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/model.int8.onnx asr/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/tokens.txt /opt/haro/models/sensevoice/
COPY --from=models asr/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20/encoder-epoch-13-avg-2-chunk-16-left-64.int8.onnx \
     asr/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20/decoder-epoch-13-avg-2-chunk-16-left-64.onnx \
     asr/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20/joiner-epoch-13-avg-2-chunk-16-left-64.int8.onnx \
     asr/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20/tokens.txt /opt/haro/models/kws/
COPY --from=models asr/silero_vad.onnx asr/smart-turn-v3.2-cpu.onnx /opt/haro/models/
COPY --from=models speaker/3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx /opt/haro/models/

# 镜像内部的路径（和镜像一起变）；环境相关的配置（TTS 地址、大模型、性格目录……）由 K8s ConfigMap 给
ENV LD_LIBRARY_PATH=/opt/haro/lib \
    HARO_SERVER_LISTEN=0.0.0.0:59130 \
    HARO_ASR_LISTEN=127.0.0.1:59126 \
    HARO_ASR_URL=http://127.0.0.1:59126 \
    HARO_ASR_MODEL_DIR=/opt/haro/models/sensevoice \
    HARO_VAD_MODEL=/opt/haro/models/silero_vad.onnx \
    HARO_TURN_MODEL=/opt/haro/models/smart-turn-v3.2-cpu.onnx \
    HARO_KWS_MODEL_DIR=/opt/haro/models/kws \
    HARO_WAKE_WORDS_FILE=/opt/haro/assets/wake-words.txt \
    HARO_SPEAKER_MODEL=/opt/haro/models/3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx \
    HARO_VOICEPRINTS_DIR=/data/voiceprints \
    HARO_NOTES_DB=/data/notes.db \
    HARO_USAGE_DB=/data/usage.db \
    HARO_RESEARCH_DB=/data/research.db \
    HARO_CONVERSATIONS_DB=/data/conversations.db \
    HARO_CONVERSATIONS_DIR=/data/conversations \
    HARO_PERSONA_STATE=/data/persona \
    HARO_MODEL_PRICES_CACHE=/data/model-prices.json

# 版本标签放在最后：ARG 之后的 RUN 都会因版本号变化而重建（apt 那一层就不能复用缓存了）
ARG VERSION=dev
LABEL org.opencontainers.image.title="haro-server" \
      org.opencontainers.image.source="https://github.com/bukahou/haro" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"
USER 10001:10001
EXPOSE 59130
ENTRYPOINT ["/opt/haro/bin/haro-server"]
