-- 金额按原始币种记：每笔存「百万分之一个货币单位」的整数和币种（ISO 4217）；换成默认货币在判断额度、汇总时做。
-- 之前的记录是按日元记的，原样标成日元（数值乘 1,000,000），不按汇率猜美元。
-- +goose Up
ALTER TABLE charges ADD COLUMN amount_micros INTEGER NOT NULL DEFAULT 0;
ALTER TABLE charges ADD COLUMN currency TEXT NOT NULL DEFAULT '';
UPDATE charges SET amount_micros = CAST(ROUND(yen * 1000000) AS INTEGER), currency = 'JPY' WHERE yen != 0;
ALTER TABLE charges DROP COLUMN yen;
ALTER TABLE cap_raises ADD COLUMN amount_micros INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cap_raises ADD COLUMN currency TEXT NOT NULL DEFAULT '';
UPDATE cap_raises SET amount_micros = CAST(ROUND(yen * 1000000) AS INTEGER), currency = 'JPY';
ALTER TABLE cap_raises DROP COLUMN yen;
