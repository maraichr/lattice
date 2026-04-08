import type { OracleMessage as OracleMessageType } from "../../stores/oracle";
import { Skeleton } from "../ui/skeleton";
import { TextBlock } from "./blocks/TextBlock";
import { OracleBlockRenderer } from "./OracleBlockRenderer";
import { OracleHints } from "./OracleHints";

interface Props {
  message: OracleMessageType;
  onHintClick: (question: string) => void;
}

export function OracleMessage({ message, onHintClick }: Props) {
  if (message.role === "user") {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-xl rounded-br-sm bg-primary/15 px-3.5 py-2.5 text-sm text-foreground">
          {message.content}
        </div>
      </div>
    );
  }

  // Oracle response
  if (message.isLoading) {
    return (
      <div className="space-y-2">
        <Skeleton className="h-4 w-3/4" />
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-20 w-full" />
      </div>
    );
  }

  // Content-only response (LLM agent path or error)
  if (message.content && !message.blocks?.length) {
    return (
      <div className="rounded-xl rounded-bl-sm bg-muted/60 px-3.5 py-2.5">
        <TextBlock data={{ content: message.content }} />
      </div>
    );
  }

  return (
    <div className="space-y-3">
      {message.tool && (
        <div className="flex items-center gap-1.5 text-[10px] text-muted-foreground">
          <span className="rounded bg-muted px-1.5 py-0.5 font-mono">{message.tool}</span>
          {message.meta && (
            <span>
              {message.meta.shown} of {message.meta.total_results} results
            </span>
          )}
        </div>
      )}
      {message.blocks?.map((block, i) => (
        <OracleBlockRenderer key={i} block={block} />
      ))}
      {message.hints && message.hints.length > 0 && (
        <OracleHints hints={message.hints} onClick={onHintClick} />
      )}
    </div>
  );
}
