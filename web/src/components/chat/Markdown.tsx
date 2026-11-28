"use client";

// haro 的回答是 Markdown（契约 ReplyBlock.text）。不渲染里面的 HTML（react-markdown 默认不渲染）；链接在新标签页打开。
// 表格：宽屏是普通的表格；手机上每一行拆成一张卡片（各项目同一个规矩），每个格子前面写上它那一列的标题（labelTableCells 记在 data-th 上）。
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

const components: Components = {
  a: ({ href, children }) => (
    <a href={href} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  ),
  table: ({ children }) => (
    <div className="table-scroll">
      <table className="cardify">{children}</table>
    </div>
  ),
};

// HAST（rehype 的语法树）里用到的那几样字段
interface HastNode {
  type: string;
  tagName?: string;
  value?: string;
  properties?: Record<string, unknown>;
  children?: HastNode[];
}

const childrenOf = (node: HastNode, ...tags: string[]) =>
  (node.children ?? []).filter((child) => child.type === "element" && tags.includes(child.tagName ?? ""));

const textOf = (node: HastNode): string => (node.type === "text" ? (node.value ?? "") : (node.children ?? []).map(textOf).join(""));

// labelTableCells 是 rehype 插件：表头每一列的文字记到下面同一列的每个格子上（data-th），手机上拆成卡片时显示在格子前面
function labelTableCells() {
  const visit = (node: HastNode) => {
    if (node.type === "element" && node.tagName === "table") {
      const headRow = childrenOf(node, "thead").flatMap((head) => childrenOf(head, "tr"))[0];
      const titles = headRow ? childrenOf(headRow, "th", "td").map(textOf) : [];
      for (const row of childrenOf(node, "tbody").flatMap((body) => childrenOf(body, "tr"))) {
        childrenOf(row, "td", "th").forEach((cell, index) => {
          if (titles[index]) cell.properties = { ...cell.properties, dataTh: titles[index] };
        });
      }
      return;
    }
    for (const child of node.children ?? []) visit(child);
  };
  return (tree: HastNode) => visit(tree);
}

export function Markdown({ text }: { text: string }) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[labelTableCells]} components={components}>
      {text}
    </ReactMarkdown>
  );
}
