import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * Merges the given class names. clsx flattens nested arrays and falsy
 * values; tailwind-merge resolves conflicts so e.g. `cn("p-2", "p-4")`
 * keeps only `p-4`.
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs));
}
