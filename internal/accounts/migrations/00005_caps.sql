-- 第 20 段：管理员给每个账号定的月上限（管理员自己用全局的上限）。NULL = 不设。
-- +goose Up
ALTER TABLE accounts ADD COLUMN monthly_cap_micros INTEGER;
ALTER TABLE accounts ADD COLUMN monthly_cap_currency TEXT;
