// 调查状态的颜色（文字由后端给，status_label）：查完绿、在查黄、没查成红。
import type { ResearchStatus } from "@/api/generated/model";
import type { TagTone } from "@/components/ui";

export const researchTone: Record<ResearchStatus, TagTone> = {
  queued: "warn",
  running: "warn",
  done: "accent",
  partial: "warn",
  failed: "danger",
  cancelled: "neutral",
  interrupted: "danger",
};
