// Markdown renderer for assistant output: a deliberately small subset (inline
// spans, headings, lists, blockquotes, rules, fenced code, pipe tables) built
// with DOM text nodes so model output is never interpreted as markup.
import { copyButton } from "./widgets.js";

const INLINE_PATTERN = /(`[^`\n]+`|\*\*[^*\n]+\*\*|__[^_\n]+__|\[([^\]\n]+)\]\((https?:\/\/[^)\s]+)\)|\*[^*\n]+\*|(?<![\w])_[^_\n]+_(?![\w]))/g;

function inlineMarkdown(element, source) {
  let offset = 0;
  for (const match of source.matchAll(INLINE_PATTERN)) {
    const token = match[0];
    const start = match.index ?? 0;
    if (start > offset) element.append(document.createTextNode(source.slice(offset, start)));
    if (token.startsWith("`")) {
      const code = document.createElement("code");
      code.textContent = token.slice(1, -1);
      element.append(code);
    } else if (token.startsWith("[")) {
      const link = document.createElement("a");
      link.textContent = match[2];
      link.href = match[3];
      link.target = "_blank";
      link.rel = "noreferrer";
      element.append(link);
    } else if (token.startsWith("**") || token.startsWith("__")) {
      const strong = document.createElement("strong");
      strong.textContent = token.slice(2, -2);
      element.append(strong);
    } else if (token.startsWith("*") || token.startsWith("_")) {
      const emphasis = document.createElement("em");
      emphasis.textContent = token.slice(1, -1);
      element.append(emphasis);
    }
    offset = start + token.length;
  }
  if (offset < source.length) element.append(document.createTextNode(source.slice(offset)));
}

// tableCells splits "| a | b |" into trimmed cells, tolerating the optional
// outer pipes. A delimiter row ("| --- | :--: |") is recognized separately so
// prose containing a single "|" still renders as a paragraph.
function tableCells(line) {
  return line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => cell.trim());
}

function delimiterAlignments(line) {
  const parts = tableCells(line);
  if (parts.length === 0 || !parts.every((part) => /^:?-+:?$/.test(part))) return undefined;
  return parts.map((part) => {
    if (part.startsWith(":") && part.endsWith(":")) return "center";
    if (part.endsWith(":")) return "right";
    if (part.startsWith(":")) return "left";
    return "";
  });
}

export function renderMarkdown(element, source) {
  element.replaceChildren();
  const lines = source.replace(/\r\n?/g, "\n").split("\n");
  let paragraph = [];
  let lists = [];
  let code;
  let fence;

  const flushParagraph = () => {
    if (paragraph.length === 0) return;
    const p = document.createElement("p");
    inlineMarkdown(p, paragraph.join(" "));
    element.append(p);
    paragraph = [];
  };
  const flushList = () => {
    lists = [];
  };
  const flushText = () => {
    flushParagraph();
    flushList();
  };

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    const opening = line.match(/^ {0,3}(`{3,}|~{3,})\s*([\w+-]*)\s*$/);
    if (fence) {
      if (opening && opening[1][0] === fence) {
        fence = undefined;
      } else {
        code.textContent += `${code.textContent ? "\n" : ""}${line}`;
      }
      continue;
    }
    if (opening) {
      flushText();
      const language = opening[2].replace(/[^\w+-]/g, "");
      const codeBlock = document.createElement("div");
      codeBlock.className = "code-block";
      const header = document.createElement("div");
      header.className = "code-header";
      const label = document.createElement("span");
      label.className = "code-language";
      label.textContent = language || "code";
      const pre = document.createElement("pre");
      const codeElement = document.createElement("code");
      code = codeElement;
      pre.append(code);
      header.append(label, copyButton("Copy code", () => codeElement.textContent || ""));
      codeBlock.append(header, pre);
      element.append(codeBlock);
      fence = opening[1][0];
      continue;
    }
    // A table is a row followed by a delimiter row of the same width. The
    // width check keeps prose ("a | b" above a --- rule) out of the table path.
    const alignments = index + 1 < lines.length ? delimiterAlignments(lines[index + 1]) : undefined;
    if (alignments && line.includes("|") && tableCells(line).length === alignments.length) {
      flushText();
      const headerCells = tableCells(line);
      const table = document.createElement("table");
      const headerRow = document.createElement("tr");
      headerCells.forEach((cell, column) => {
        const th = document.createElement("th");
        if (alignments[column]) th.style.textAlign = alignments[column];
        inlineMarkdown(th, cell);
        headerRow.append(th);
      });
      const head = document.createElement("thead");
      head.append(headerRow);
      const body = document.createElement("tbody");
      let row = index + 2;
      for (; row < lines.length && lines[row].includes("|") && !delimiterAlignments(lines[row]); row += 1) {
        const cells = tableCells(lines[row]);
        const tr = document.createElement("tr");
        for (let column = 0; column < headerCells.length; column += 1) {
          const td = document.createElement("td");
          if (alignments[column]) td.style.textAlign = alignments[column];
          inlineMarkdown(td, cells[column] || "");
          tr.append(td);
        }
        body.append(tr);
      }
      table.append(head, body);
      element.append(table);
      index = row - 1;
      continue;
    }
    if (!line.trim()) {
      flushText();
      continue;
    }
    const heading = line.match(/^ {0,3}(#{1,6})\s+(.+?)\s*#*$/);
    if (heading) {
      flushText();
      const h = document.createElement(`h${heading[1].length}`);
      inlineMarkdown(h, heading[2]);
      element.append(h);
      continue;
    }
    if (/^ {0,3}([-*_])(?:\s*\1){2,}\s*$/.test(line)) {
      flushText();
      element.append(document.createElement("hr"));
      continue;
    }
    const bullet = line.match(/^(\s*)[-+*]\s+(.+)$/);
    const ordered = line.match(/^(\s*)\d+[.)]\s+(.+)$/);
    if (bullet || ordered) {
      flushParagraph();
      const kind = ordered ? "ol" : "ul";
      const match = ordered || bullet;
      const indent = match[1].length;
      while (lists.length && lists[lists.length - 1].indent > indent) lists.pop();
      let current = lists[lists.length - 1];
      if (!current || current.indent !== indent || current.kind !== kind) {
        if (current?.indent === indent) lists.pop();
        const parent = lists[lists.length - 1];
        const list = document.createElement(kind);
        (parent?.item || element).append(list);
        current = { indent, kind, item: undefined, list };
        lists.push(current);
      }
      const item = document.createElement("li");
      inlineMarkdown(item, match[2]);
      current.list.append(item);
      current.item = item;
      continue;
    }
    const continuation = line.match(/^(\s+)(.+)$/);
    if (continuation && lists.length && lists[lists.length - 1].item) {
      const item = lists[lists.length - 1].item;
      item.append(document.createTextNode(" "));
      inlineMarkdown(item, continuation[2].trim());
      continue;
    }
    if (lists.length) {
      flushList();
    }
    const quote = line.match(/^ {0,3}>\s?(.*)$/);
    if (quote) {
      flushText();
      const blockquote = document.createElement("blockquote");
      inlineMarkdown(blockquote, quote[1]);
      element.append(blockquote);
      continue;
    }
    flushList();
    paragraph.push(line.trim());
  }
  flushText();
}
