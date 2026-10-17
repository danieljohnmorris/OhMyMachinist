import { cva } from "class-variance-authority";
import { cn } from "@/lib/utils";

// Tones carry meaning, never decoration: active work is primary, outcomes are
// success/danger, things waiting on a human are warning, everything else neutral.
export const badgeVariants = cva("inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-semibold capitalize", {
  variants: {
    tone: {
      neutral: "border-border bg-muted text-muted-foreground",
      surface: "border-border bg-surface text-muted-foreground",
      active: "border-primary/25 bg-primary/10 text-primary",
      success: "border-success/25 bg-success/10 text-success",
      warning: "border-warning/28 bg-warning/10 text-warning",
      danger: "border-danger/25 bg-danger/10 text-danger",
    },
  },
  defaultVariants: { tone: "neutral" },
});

export function Badge({ className, tone, dot = false, children, ...props }) {
  return <span className={cn(badgeVariants({ tone }), className)} {...props}>
    {dot && <span className="size-1.5 rounded-full bg-current" aria-hidden="true" />}
    {children}
  </span>;
}
