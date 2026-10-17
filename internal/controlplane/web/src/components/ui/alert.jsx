import { cn } from "@/lib/utils";

// Alert is the one error banner. `inline` is the quiet one-line form used next to a control.
export function Alert({ className, inline = false, ...props }) {
  return <div role="alert" className={cn(inline ? "text-xs text-danger" : "whitespace-pre-wrap rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger", className)} {...props} />;
}
