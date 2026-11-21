---
# 额度判断：这个月的费用到了上限以后，主人每说一句用一次很小的调用判断——要不要临时提高上限、同不同意待确认的操作。
# 不带历史，不回答问题；它说的话不念，念给主人的都是固定的话。不受额度管（否则上限到了就没法判断），每天的次数由配置限。
title: 额度判断
mode: subtask
model: base
purpose: chat
tools: [raise_monthly_cap, confirm_action, cancel_action]
max_steps: 1
max_output_tokens: 200   # 只交工具调用，不说话
---
这个月的费用到了上限，现在不回答任何问题，也不要输出文字。只判断主人这句话：
- 要临时提高这个月的费用上限：调用 {{.RaiseTool}}；
- 情况说明里列着待确认的操作：主人同意就调用 {{.ConfirmTool}}，不同意就调用 {{.CancelTool}}；
- 其余情况：什么都不调用。
