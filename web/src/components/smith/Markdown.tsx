import type { ReactNode } from "react";
import { CopyButton } from "../CopyButton";

// Markdown — a hand-rolled, safe-subset renderer for smith chat content
// (docs/v5-smith.md §10 risk #4): headings, **bold**, *italic*/_italic_,
// `inline code`, fenced ```code blocks```, lists, and http(s) links.
// Deliberately not a CommonMark implementation, and deliberately never
// touches dangerouslySetInnerHTML — every node here is a real React
// element built from parsed substrings, so there is no HTML-injection
// surface for LLM-authored text to land in (this app has zero
// dangerouslySetInnerHTML usage anywhere; this file keeps it that way).
// Nested inline styles (e.g. bold inside a link label) render as literal
// markers rather than nesting — an acceptable v1 limitation for a
// diagnostic chat transcript, not a general-purpose renderer.

const INLINE_RE = /\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)|`([^`]+)`|\*\*([^*]+)\*\*|\*([^*]+)\*|_([^_]+)_/g;

function renderInline(text: string, keyPrefix: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let i = 0;
  INLINE_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = INLINE_RE.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const key = `${keyPrefix}-${i++}`;
    if (m[1] !== undefined && m[2] !== undefined) {
      out.push(
        <a key={key} href={m[2]} target="_blank" rel="noopener noreferrer">
          {m[1]}
        </a>
      );
    } else if (m[3] !== undefined) {
      out.push(<code key={key}>{m[3]}</code>);
    } else if (m[4] !== undefined) {
      out.push(<strong key={key}>{m[4]}</strong>);
    } else if (m[5] !== undefined) {
      out.push(<em key={key}>{m[5]}</em>);
    } else if (m[6] !== undefined) {
      out.push(<em key={key}>{m[6]}</em>);
    }
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

function Heading({ level, children }: { level: number; children: ReactNode }) {
  // Offset so a chat heading never competes visually with the page's own
  // h1/h2 chrome — level 1 lands at h4, deeper levels flatten to h6.
  if (level <= 1) return <h4>{children}</h4>;
  if (level === 2) return <h5>{children}</h5>;
  return <h6>{children}</h6>;
}

function CodeBlock({ lang, code }: { lang: string; code: string }) {
  const trimmed = code.replace(/\n$/, "");
  return (
    <div className="smith-code-block">
      {lang && <div className="smith-code-lang">{lang}</div>}
      <pre>
        <code>{trimmed}</code>
      </pre>
      <CopyButton text={trimmed} title="Copy code" sm />
    </div>
  );
}

// renderTextBlocks handles everything except fenced code (already split out
// by the caller): headings, paragraphs (blank-line separated), and
// consecutive `- `/`* `/`1. ` lines grouped into one list.
function renderTextBlocks(text: string, keyPrefix: string): ReactNode[] {
  const lines = text.split("\n");
  const out: ReactNode[] = [];
  let para: string[] = [];
  let list: { ordered: boolean; items: string[] } | null = null;
  let idx = 0;

  function flushPara() {
    if (para.length > 0) {
      const key = `${keyPrefix}-p${idx++}`;
      out.push(<p key={key}>{renderInline(para.join(" "), key)}</p>);
      para = [];
    }
  }
  function flushList() {
    if (list) {
      const key = `${keyPrefix}-l${idx++}`;
      const items = list.items.map((item, i) => <li key={`${item.slice(0, 32)}-${i}`}>{renderInline(item, `${key}-${i}`)}</li>);
      out.push(list.ordered ? <ol key={key}>{items}</ol> : <ul key={key}>{items}</ul>);
      list = null;
    }
  }

  for (const raw of lines) {
    const line = raw.trimEnd();
    if (line.trim() === "") {
      flushPara();
      flushList();
      continue;
    }
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      flushPara();
      flushList();
      const key = `${keyPrefix}-h${idx++}`;
      out.push(
        <Heading key={key} level={heading[1].length}>
          {renderInline(heading[2], key)}
        </Heading>
      );
      continue;
    }
    const ul = /^[-*]\s+(.*)$/.exec(line);
    const ol = /^\d+\.\s+(.*)$/.exec(line);
    if (ul || ol) {
      flushPara();
      const ordered = !!ol;
      const itemText = (ul ?? ol)![1];
      if (!list || list.ordered !== ordered) {
        flushList();
        list = { ordered, items: [] };
      }
      list.items.push(itemText);
      continue;
    }
    flushList();
    para.push(line);
  }
  flushPara();
  flushList();
  return out;
}

const FENCE_RE = /```(\w*)\n([\s\S]*?)```/g;

export function Markdown({ text }: { text: string }) {
  if (!text) return null;
  const blocks: ReactNode[] = [];
  let last = 0;
  let n = 0;
  FENCE_RE.lastIndex = 0;
  let m: RegExpExecArray | null;
  while ((m = FENCE_RE.exec(text))) {
    if (m.index > last) blocks.push(...renderTextBlocks(text.slice(last, m.index), `b${n++}`));
    blocks.push(<CodeBlock key={`fence-${n++}`} lang={m[1]} code={m[2]} />);
    last = m.index + m[0].length;
  }
  if (last < text.length) blocks.push(...renderTextBlocks(text.slice(last), `b${n++}`));
  return <div className="smith-markdown">{blocks}</div>;
}
