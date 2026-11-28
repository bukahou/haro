"use client";

// 记账的分类（固定的几类 + 自己加的；名字按语言由后端给）。
import { listExpenseCategories } from "@/api/generated/expenses/expenses";
import { useApi } from "@/lib/useApi";

const loadCategories = () => listExpenseCategories();

export function useCategories() {
  return useApi(loadCategories).data ?? [];
}
