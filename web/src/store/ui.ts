import { create } from "zustand";

/**
 * The state of the window itself, as opposed to the state of the work.
 *
 * It is kept apart from the tabs on purpose. The window is one and the tabs are many,
 * and a thing that belongs to the window should not be re-rendered because a tab
 * changed. Collapsing the sidebar repaints one column; it has no business touching
 * the contents of a conversation somebody is reading.
 */
export type Theme = "light" | "dark" | "system";

type UIState = {
  /** Whether the task menu is shown. Collapsed means gone, not narrow. */
  leftCollapsed: boolean;
  /** The task menu's width in pixels, because a person dragged it there. */
  sidebarWidth: number;
  /** Whether the right dock is shown. */
  rightOpen: boolean;
  theme: Theme;
  toggleLeft: () => void;
  setLeftCollapsed: (collapsed: boolean) => void;
  setSidebarWidth: (width: number) => void;
  toggleRight: () => void;
  setTheme: (theme: Theme) => void;
};

const sidebarWidthKey = "marxagent.sidebarWidth";
const themeKey = "marxagent.theme";

/**
 * The width limits.
 *
 * A sidebar narrower than this cannot show a project name and its count at the same
 * time, and one wider than this pushes the work area off a laptop screen. Both ends
 * are therefore a decision rather than a clamp that happens.
 */
const minWidth = 180;
const maxWidth = 420;

function readNumber(key: string, fallback: number): number {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return fallback;
    const value = Number(raw);
    return Number.isFinite(value) ? value : fallback;
  } catch {
    // Storage can be refused, and a window with a default width is a far smaller
    // problem than a window that failed to start over a preference.
    return fallback;
  }
}

function readTheme(): Theme {
  try {
    const raw = window.localStorage.getItem(themeKey);
    if (raw === "light" || raw === "dark") return raw;
  } catch {
    // As above: a wrong theme is survivable, a page that did not render is not.
  }
  return "system";
}

export const useUI = create<UIState>((set) => ({
  leftCollapsed: false,
  sidebarWidth: readNumber(sidebarWidthKey, 232),
  rightOpen: false,
  theme: readTheme(),

  toggleLeft: () => set((state) => ({ leftCollapsed: !state.leftCollapsed })),
  setLeftCollapsed: (leftCollapsed) => set({ leftCollapsed }),
  setSidebarWidth: (width) => {
    const clamped = Math.min(maxWidth, Math.max(minWidth, width));
    set({ sidebarWidth: clamped });
    // Written on every move of the drag, because a width somebody chose and lost on
    // the next launch is a width they will choose again.
    try {
      window.localStorage.setItem(sidebarWidthKey, String(clamped));
    } catch {
      // Nothing to do; the width still holds for this window.
    }
  },
  toggleRight: () => set((state) => ({ rightOpen: !state.rightOpen })),
  setTheme: (theme) => {
    set({ theme });
    applyTheme(theme);
    try {
      window.localStorage.setItem(themeKey, theme);
    } catch {
      // As above.
    }
  },
}));

/**
 * Put the theme on the document.
 *
 * The attribute is what the stylesheet keys off, and it is set here as well as in the
 * document's own early script. The early script runs before anything is painted; this
 * runs when a person changes the theme, which is after. Both write the same value to
 * the same place, so there is one answer rather than two that can disagree.
 */
export function applyTheme(theme: Theme): void {
  const dark =
    theme === "dark" ||
    (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  const root = document.documentElement;
  root.setAttribute("data-theme", dark ? "dark" : "light");
  root.style.colorScheme = dark ? "dark" : "light";
}

/** The resolved theme, for the case where something needs to know rather than show. */
export function resolvedTheme(theme: Theme): "light" | "dark" {
  return theme === "system"
    ? window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark"
      : "light"
    : theme;
}
