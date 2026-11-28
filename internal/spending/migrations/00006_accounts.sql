-- 第 20 段：每笔花费记上是哪个账号的（按账号的月上限判断、按账号汇总）。空 = 服务端主人（管理员）的；以前的记录都算管理员的。
-- +goose Up
ALTER TABLE charges ADD COLUMN account TEXT NOT NULL DEFAULT '';
CREATE INDEX charges_account_at ON charges (account, at);

-- +goose Down
DROP INDEX charges_account_at;
ALTER TABLE charges DROP COLUMN account;
