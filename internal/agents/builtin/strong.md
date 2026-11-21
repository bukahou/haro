---
# 强模型：前台判断自己答不好时（主人纠正、或明说要想清楚）交给它，带着完整的对话历史回答这一轮剩下的部分，
# 它的话直接说给主人。看画面用接手的方式时（HARO_STRONG_LLM_VISION_MODE=takeover），带画面的请求也交给它。
# 沿用对话的系统提示词（性格、规则、工具介绍都和前台一样），所以正文留空。
# 它出错或等太久（HARO_STRONG_LLM_TIMEOUT）时由前台的模型接手。
title: 强模型
mode: handoff
model: strong
purpose: strong
max_steps: 8
repeat_limit: 3
repeat_window: 4
---
