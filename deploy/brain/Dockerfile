# syntax=docker/dockerfile:1
# haro-brain 镜像（v4 第 4 步）：大脑（含记事本等内置工具，v5）+ haro-asr（cgo，sherpa-onnx），一个容器、一个 Pod。
#
# 构建（在仓库根目录；模型不在仓库里，用命名构建上下文 models 指向本机的模型目录）：
#   make image          # = docker buildx build -f deploy/brain/Dockerfile --build-context models=$HOME/.local/share/haro .
#
# 镜像里有什么 / 没什么（v4 4-1 b：公开仓库）：
#   ✅ 两个程序、识别 / 唤醒 / 声纹模型（都是公开模型）、唤醒词文件
#   ⛔ 性格档案、声音档案（由部署方的 ConfigMap 挂到 /etc/haro）、任何凭证（K8s Secret）、
#      声纹 / 记事 / 对话（数据卷 /data）
#
# 只做 x86（v4 D6）。

FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# haro-brain 纯 Go；haro-asr 要 cgo（sherpa-onnx 的原生库随 Go 模块下载，运行时用 LD_LIBRARY_PATH 找）
RUN CGO_ENABLED=0 go build -trimpath -o /out/bin/haro-brain ./cmd/haro-brain \
 && go build -trimpath -o /out/bin/haro-asr ./cmd/haro-asr \
 && mkdir -p /out/lib \
 && cp "$(go list -m -f '{{.Dir}}' github.com/k2-fsa/sherpa-onnx-go-linux)"/lib/x86_64-unknown-linux-gnu/*.so /out/lib/

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --uid 10001 --user-group --no-create-home --shell /usr/sbin/nologin haro \
 && mkdir -p /data /etc/haro && chown haro:haro /data
COPY --from=build /out/bin/ /opt/haro/bin/
COPY --from=build /out/lib/ /opt/haro/lib/
COPY assets/wake-words.txt /opt/haro/assets/wake-words.txt
# 模型（只拷用到的文件：SenseVoice 用 int8 版）。声纹模型保留原文件名：声纹记录里存的是模型文件名，换名字旧声纹就对不上了
COPY --from=models asr/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/model.int8.onnx asr/sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17/tokens.txt /opt/haro/models/sensevoice/
COPY --from=models asr/sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20/ /opt/haro/models/kws/
COPY --from=models asr/silero_vad.onnx asr/smart-turn-v3.2-cpu.onnx /opt/haro/models/
COPY --from=models speaker/3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced.onnx /opt/haro/models/

# 镜像内部的路径（和镜像一起变）；环境相关的配置（TTS 地址、大模型、性格目录……）由 K8s ConfigMap 给
ENV LD_LIBRARY_PATH=/opt/haro/lib \
    HARO_BRAIN_LISTEN=0.0.0.0:59130 \
    HARO_ASR=local \
    HARO_ASR_BIN=/opt/haro/bin/haro-asr \
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
    HARO_CONVERSATIONS_DIR=/data/conversations \
    HARO_PERSONA_STATE=/data/persona

USER 10001:10001
EXPOSE 59130
ENTRYPOINT ["/opt/haro/bin/haro-brain"]
