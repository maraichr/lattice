import { Sparkles } from "lucide-react";

interface Props {
  onQuestionClick: (question: string) => void;
}

const suggestedQuestions = [
  "Give me a project overview",
  "What are the most important tables?",
  "Show table relationships",
  "Trace the full stack from app code to database",
];

export function OracleWelcome({ onQuestionClick }: Props) {
  return (
    <div className="flex flex-col items-center justify-center py-12 text-center">
      <div className="mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-primary/10">
        <Sparkles className="h-6 w-6 text-primary" />
      </div>
      <h3 className="text-sm font-semibold text-foreground mb-1">Ask The Oracle</h3>
      <p className="text-xs text-muted-foreground mb-6 max-w-[280px]">
        Ask questions about your codebase. I'll find the right data and show you structured results.
      </p>
      <div className="w-full space-y-2">
        {suggestedQuestions.map((q) => (
          <button
            key={q}
            onClick={() => onQuestionClick(q)}
            className="w-full rounded-lg border border-border bg-muted/30 px-3 py-2.5 text-left text-xs text-muted-foreground transition-colors hover:bg-muted/60 hover:text-foreground"
          >
            {q}
          </button>
        ))}
      </div>
    </div>
  );
}
