# 服务端镜像里的模型

> The haro-server image and the `models-v1` release of this repository ship the public models below,
> byte-for-byte identical to the upstream files. Checksums: [models.lock](models.lock);
> license texts: [MODEL-LICENSES.txt](MODEL-LICENSES.txt).

haro-server 镜像（以及本仓库的 Release `models-v1`）里带着下面这些公开模型。文件和原发布方的完全一样
（压缩包里的只是解出来，没有改动）；sha256 与原发布方地址见 [models.lock](models.lock)，许可证全文见
[MODEL-LICENSES.txt](MODEL-LICENSES.txt)。

| 用途 | 模型 | 作者 / 原发布方 | 许可证 | 用到的文件 |
|---|---|---|---|---|
| 语音识别 | SenseVoice Small（`sherpa-onnx-sense-voice-zh-en-ja-ko-yue-2024-07-17`，int8 量化） | 阿里巴巴集团 FunAudioLLM / FunASR；ONNX 转换：k2-fsa sherpa-onnx | FunASR Model Open Source License v1.1 | `model.int8.onnx`、`tokens.txt` |
| 唤醒词 | `sherpa-onnx-kws-zipformer-zh-en-3M-2025-12-20` | k2-fsa（icefall / sherpa-onnx） | Apache-2.0（据 ModelScope 原模型页 `pkufool/icefall-kws-zipformer-zh-en-3M-2025-12-20`） | chunk-16 的 encoder / decoder / joiner、`tokens.txt` |
| 说话起止 | Silero VAD | Silero Team | MIT | `silero_vad.onnx` |
| 话说完了没有 | Smart Turn v3.2（CPU 版） | Daily（pipecat-ai） | BSD-2-Clause | `smart-turn-v3.2-cpu.onnx` |
| 声纹 | CAM++（`3dspeaker_speech_campplus_sv_zh_en_16k-common_advanced`） | 阿里巴巴集团 3D-Speaker；ONNX 转换：k2-fsa sherpa-onnx | Apache-2.0 | 同名 `.onnx` |

## 为什么放在自己的 Release 里

原发布方随时可能删文件或换内容。第一次从原发布方下载、按 `models.lock` 的 sha256 校验后，传到本仓库的
Release 留底；之后构建镜像只从本仓库的 Release 下载（同样校验）。

```sh
scripts/fetch-models.sh ~/.local/share/haro                  # 从本仓库的 Release 下载（已有且校验通过的文件跳过）
scripts/fetch-models.sh --from-upstream ~/.local/share/haro  # 从原发布方下载（建 Release 时用）
```

换模型：改 `models.lock`（新的 sha256、大小、地址），把 `release` 改成下一个标签（`models-v2`），提交后推这个标签
（或在 Actions 页面手动运行 models）：`.github/workflows/models.yml` 会从原发布方下载、校验，建好新的 Release
（旧的不动，旧镜像仍能重建）。
