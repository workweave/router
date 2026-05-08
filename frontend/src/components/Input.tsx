import { cn } from "@/lib/cn";
import { forwardRef, type InputHTMLAttributes } from "react";

interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  label?: string;
  error?: string;
  hint?: string;
}

const FIELD =
  "flex h-9 w-full rounded-md border border-input bg-background px-3 py-1 text-sm shadow-sm transition-colors placeholder:text-muted-foreground focus-visible:border-foreground/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50";

export const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ label, error, hint, className, id, ...props }, ref) => {
    const inputId = id ?? label?.toLowerCase().replace(/\s+/g, "-");
    return (
      <div className="flex flex-col gap-1.5">
        {label != null && (
          <label htmlFor={inputId} className="text-xs font-medium text-foreground">
            {label}
          </label>
        )}
        <input ref={ref} id={inputId} className={cn(FIELD, className)} {...props} />
        {hint != null && error == null && <p className="text-2xs text-muted-foreground">{hint}</p>}
        {error != null && <p className="text-2xs text-danger">{error}</p>}
      </div>
    );
  },
);
Input.displayName = "Input";
