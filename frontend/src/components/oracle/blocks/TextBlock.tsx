import { Check, Clipboard } from "lucide-react";
import { type ComponentPropsWithoutRef, useCallback, useState } from "react";
import Markdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { OracleTextData } from "../../../api/types";

interface Props {
  data: unknown;
}

export function TextBlock({ data }: Props) {
  const d = data as OracleTextData;
  if (!d.content) return null;

  return (
    <div className="oracle-markdown text-xs leading-relaxed text-muted-foreground">
      <Markdown remarkPlugins={[remarkGfm]} components={markdownComponents}>
        {d.content}
      </Markdown>
    </div>
  );
}

function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }, [text]);

  return (
    <button
      type="button"
      onClick={handleCopy}
      className="absolute right-2 top-2 flex h-6 w-6 items-center justify-center rounded bg-muted-foreground/10 text-muted-foreground opacity-0 transition-all hover:bg-muted-foreground/20 hover:text-foreground group-hover/code:opacity-100"
      title="Copy code"
    >
      {copied ? <Check className="h-3 w-3 text-emerald-400" /> : <Clipboard className="h-3 w-3" />}
    </button>
  );
}

function CodeBlock({
  children,
  className,
}: ComponentPropsWithoutRef<"code"> & { children?: React.ReactNode }) {
  const isInline = !className && typeof children === "string" && !children.includes("\n");

  if (isInline) {
    return (
      <code className="rounded bg-muted px-1 py-0.5 text-[11px] font-mono text-foreground/90">
        {children}
      </code>
    );
  }

  const language = className?.replace("language-", "") ?? "";
  const text = String(children).replace(/\n$/, "");

  return (
    <div className="group/code relative my-2 overflow-hidden rounded-md border border-border/50 bg-[oklch(0.16_0.01_260)]">
      {language && (
        <div className="flex items-center border-b border-border/30 bg-muted/30 px-3 py-1">
          <span className="text-[10px] font-mono text-muted-foreground/70">{language}</span>
        </div>
      )}
      <CopyButton text={text} />
      <pre className="overflow-x-auto p-3 text-[11px] leading-[1.6]">
        <code className="font-mono text-foreground/85">{text}</code>
      </pre>
    </div>
  );
}

const markdownComponents = {
  // Headings - compact for side panel
  h1: ({ children }: ComponentPropsWithoutRef<"h1">) => (
    <h1 className="mb-2 mt-3 text-sm font-semibold text-foreground first:mt-0">{children}</h1>
  ),
  h2: ({ children }: ComponentPropsWithoutRef<"h2">) => (
    <h2 className="mb-1.5 mt-3 text-[13px] font-semibold text-foreground first:mt-0">{children}</h2>
  ),
  h3: ({ children }: ComponentPropsWithoutRef<"h3">) => (
    <h3 className="mb-1 mt-2.5 text-xs font-semibold text-foreground first:mt-0">{children}</h3>
  ),
  h4: ({ children }: ComponentPropsWithoutRef<"h4">) => (
    <h4 className="mb-1 mt-2 text-xs font-medium text-foreground first:mt-0">{children}</h4>
  ),

  // Paragraphs
  p: ({ children }: ComponentPropsWithoutRef<"p">) => <p className="mb-2 last:mb-0">{children}</p>,

  // Strong / emphasis
  strong: ({ children }: ComponentPropsWithoutRef<"strong">) => (
    <strong className="font-semibold text-foreground">{children}</strong>
  ),
  em: ({ children }: ComponentPropsWithoutRef<"em">) => (
    <em className="italic text-foreground/80">{children}</em>
  ),

  // Code (inline + block via pre)
  code: CodeBlock,
  pre: ({ children }: ComponentPropsWithoutRef<"pre">) => <>{children}</>,

  // Lists
  ul: ({ children }: ComponentPropsWithoutRef<"ul">) => (
    <ul className="mb-2 ml-3.5 list-disc space-y-0.5 marker:text-muted-foreground/50 last:mb-0">
      {children}
    </ul>
  ),
  ol: ({ children }: ComponentPropsWithoutRef<"ol">) => (
    <ol className="mb-2 ml-3.5 list-decimal space-y-0.5 marker:text-muted-foreground/50 last:mb-0">
      {children}
    </ol>
  ),
  li: ({ children }: ComponentPropsWithoutRef<"li">) => <li className="pl-0.5">{children}</li>,

  // Links
  a: ({ href, children }: ComponentPropsWithoutRef<"a">) => (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="text-primary underline decoration-primary/30 underline-offset-2 transition-colors hover:decoration-primary/70"
    >
      {children}
    </a>
  ),

  // Blockquote
  blockquote: ({ children }: ComponentPropsWithoutRef<"blockquote">) => (
    <blockquote className="my-2 border-l-2 border-primary/40 pl-3 text-muted-foreground/80 italic">
      {children}
    </blockquote>
  ),

  // Horizontal rule
  hr: () => <hr className="my-3 border-border/50" />,

  // Tables (GFM)
  table: ({ children }: ComponentPropsWithoutRef<"table">) => (
    <div className="my-2 overflow-x-auto rounded-md border border-border/50">
      <table className="w-full text-[11px]">{children}</table>
    </div>
  ),
  thead: ({ children }: ComponentPropsWithoutRef<"thead">) => (
    <thead className="border-b border-border/50 bg-muted/30">{children}</thead>
  ),
  th: ({ children }: ComponentPropsWithoutRef<"th">) => (
    <th className="px-2.5 py-1.5 text-left font-medium text-muted-foreground">{children}</th>
  ),
  td: ({ children }: ComponentPropsWithoutRef<"td">) => (
    <td className="border-t border-border/30 px-2.5 py-1.5 text-foreground/90">{children}</td>
  ),

  // Task lists (GFM)
  input: ({ checked, ...props }: ComponentPropsWithoutRef<"input">) => (
    <input
      type="checkbox"
      checked={checked}
      readOnly
      className="mr-1.5 h-3 w-3 rounded border-border accent-primary"
      {...props}
    />
  ),
} as const;
