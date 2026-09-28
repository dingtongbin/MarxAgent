import * as React from "react";
import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "border-brd bg-bg placeholder:text-txt-faint w-full rounded-md border px-2.5 py-1.5 text-sm",
        // The field's own text is set here rather than inherited, because a field
        // that silently used a different size from the button beside it is the most
        // common way a form looks assembled rather than designed.
        "transition-colors duration-150",
        "focus-visible:border-accent focus-visible:ring-accent/30 focus-visible:ring-2 focus-visible:outline-none",
        "disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
