# Security

## 设计上的前提 / By design

- 大脑（`haro-brain`）和语音识别服务（`haro-asr`）的接口**没有鉴权、没有加密**：只在你信任的局域网里运行，
  不要暴露到公网。局域网里能连到大脑的设备，都能和它对话、使用它的工具、消耗大模型的额度。
- 声纹是生物特征：只存在大脑本机的数据目录里，请自己控制这个目录的访问与备份。
- 云端大模型、联网搜索的密钥只从环境变量读取；不要写进仓库。

The brain and ASR endpoints are **unauthenticated and unencrypted** by design — run them only on a trusted LAN.
Voiceprints are biometric data and stay in the brain's local data directory. API keys are read from environment variables only.

## 报告漏洞 / Reporting a vulnerability

请用 GitHub 的 **私密漏洞报告**（本仓库 Security 页 → *Report a vulnerability*），**不要**公开发 issue。
Please use GitHub's **private vulnerability reporting** (Security tab → *Report a vulnerability*); do not open a public issue.
