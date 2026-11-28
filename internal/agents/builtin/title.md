---
# 起标题：App 里新建的对话，第一轮答完后在后台给它起一个简短的标题（对话列表里显示）。按「对话摘要」的用途记账。
# {{.TitleChars}} 是标题最多几个字（代码里定义一处）。
title: 起标题
mode: subtask
model: base
purpose: summary
tools: []
max_steps: 1
max_output_tokens: 60
---
给一段聊天起一个简短的标题，让人在对话列表里一眼认出聊的是什么：不超过 {{.TitleChars}} 个字，用这段聊天的语言写。
只输出标题本身：不加引号、书名号、句号，不写「标题：」。
