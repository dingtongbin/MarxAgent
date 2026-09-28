import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/**
 * Joins class names, letting a caller override a component's own classes.
 *
 * Plain joining is not enough. A component that sets its own padding and a caller
 * that asks for different padding both end up in the class list, and which one wins
 * is decided by the order the two happened to be written in — which is decided by
 * something nobody is looking at. Merging resolves the conflict by looking at what
 * each class actually sets, so the caller's class wins because the caller is later,
 * not by luck.
 */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
