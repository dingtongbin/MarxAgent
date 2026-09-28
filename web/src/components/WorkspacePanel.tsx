import { useEffect, useState } from "react";
import {
  Folder,
  FolderOpen,
  GitBranch,
  Terminal,
  ChevronRight,
  X,
} from "lucide-react";
import { browseDir, gitStatus, type DirEntry, type GitView } from "@/api";
import s from "./WorkspacePanel.module.css";

/**
 * The four kinds of tab the workspace holds, in the order they are offered.
 *
 * The navigation bar's tool buttons read this same list, so the bar and the
 * panel cannot disagree about what a tool opens, and neither is ever drawn
 * backwards.
 */
export const panelTabs = [
  { value: "files", label: "文件", icon: null },
  { value: "git", label: "git", icon: GitBranch },
  { value: "workflow", label: "workflow", icon: WorkflowIcon },
  { value: "terminal", label: "本地终端", icon: Terminal },
] as const;

export type PanelTab = (typeof panelTabs)[number]["value"];

/** One page of the workspace: a tab a person opened, and can close. */
export type PanelPage = {
  id: string;
  kind: PanelTab;
};

function panelLabel(kind: PanelTab): string {
  return panelTabs.find((entry) => entry.value === kind)?.label ?? kind;
}

function panelIcon(kind: PanelTab) {
  const found = panelTabs.find((entry) => entry.value === kind);
  return found?.icon ?? null;
}

// Workflow has no icon of its own that lucide names to match; a flat mark from
// the same set is drawn in its seat.
function WorkflowIcon({ size = 13, strokeWidth = 2 }: { size?: number; strokeWidth?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth={strokeWidth} strokeLinecap="round" strokeLinejoin="round">
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="9" y="14" width="7" height="7" rx="1" />
      <path d="M6.5 10v2a2 2 0 0 0 2 2H9" />
      <path d="M17.5 10v2a2 2 0 0 1-2 2h-.5" />
    </svg>
  );
}

/**
 * The workspace panel.
 *
 * It holds pages a person opened rather than a fixed row of four: every tool
 * click opens a page, every page can be closed, and a panel with no pages left
 * is a panel that has closed. The pages are the tab manager's own shape, one
 * level down.
 */
export function WorkspacePanel({
  project,
  pages,
  activeId,
  onActivate,
  onClosePage,
}: {
  project: string;
  pages: PanelPage[];
  activeId: string | null;
  onActivate: (id: string) => void;
  onClosePage: (id: string) => void;
}) {
  const active = pages.find((page) => page.id === activeId) ?? pages[pages.length - 1];

  return (
    <aside className={s.panel}>
      {/* The panel's own strip, with a close on every page it holds. */}
      <div className={s.panelBar}>
        <div className={s.panelTabs}>
          {pages.map((page) => {
            const Icon = panelIcon(page.kind);
            return (
              <div
                key={page.id}
                className={`${s.panelTab} ${page.id === active?.id ? s.panelTabOn : ""}`}
                onClick={() => onActivate(page.id)}
                role="tab"
                aria-selected={page.id === activeId}
                title={panelLabel(page.kind)}
              >
                {Icon ? <Icon size={13} strokeWidth={2} /> : null}
                <span className={s.panelTabLabel}>{panelLabel(page.kind)}</span>
                <button
                  className={s.pageClose}
                  onClick={(event) => {
                    event.stopPropagation();
                    onClosePage(page.id);
                  }}
                  title="关闭这个页签"
                >
                  <X size={11} strokeWidth={2} />
                </button>
              </div>
            );
          })}
        </div>
      </div>

      {/* Only the active page is mounted. A hidden page holding its tree and its
          effects is one window's worth of work to show one page of work. */}
      {active ? (
        <div className={s.panelBody} key={active.id}>
          {active.kind === "files" ? <FilesPane root={project} /> : null}
          {active.kind === "git" ? <GitPane root={project} /> : null}
          {active.kind === "workflow" ? <EmptyPane text="这个工作区还没有 workflow。" /> : null}
          {active.kind === "terminal" ? <EmptyPane text="本地终端还没有接入。" /> : null}
        </div>
      ) : null}
    </aside>
  );
}

/* --- Files: the project's tree, opened a directory at a time --- */

type TreeRow = { entry: DirEntry; depth: number };

function FilesPane({ root }: { root: string }) {
  const [rows, setRows] = useState<TreeRow[]>([]);
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");

  // The tree starts at the project root and grows only where it is opened.
  useEffect(() => {
    let cancelled = false;
    if (!root) {
      setState("ready");
      return;
    }
    browseDir(root)
      .then((listing) => {
        if (cancelled) return;
        setRows(listing.entries.map((entry) => ({ entry, depth: 0 })));
        setState("ready");
      })
      .catch(() => {
        if (!cancelled) setState("ready");
      });
    return () => {
      cancelled = true;
    };
  }, [root]);

  const under = (row: TreeRow, other: TreeRow) =>
    other.depth > row.depth && other.entry.path.startsWith(row.entry.path + "/");

  // One directory opens and closes in place: opening asks the server once,
  // closing drops what was under it, and nothing else is read.
  const toggle = async (row: TreeRow) => {
    const opened = rows.some((other) => under(row, other));
    const kept = rows.filter((other) => !under(row, other));
    if (opened) {
      setRows(kept);
      return;
    }
    try {
      const listing = await browseDir(row.entry.path);
      const children = listing.entries.map((entry) => ({ entry, depth: row.depth + 1 }));
      const index = kept.findIndex((other) => other.entry.path === row.entry.path);
      setRows([...kept.slice(0, index + 1), ...children, ...kept.slice(index + 1)]);
    } catch {
      // A directory that could not be read stays a row; it opens to nothing
      // rather than taking the tree down with it.
    }
  };

  if (!root) return <EmptyPane text="这个会话没有项目目录。" />;
  if (state === "loading") return <EmptyPane text="正在读取目录…" />;

  return (
    <div className={s.tree} role="tree">
      {rows.map((row) => {
        const opened = rows.some((other) => under(row, other));
        return (
          <button
            key={row.entry.path}
            className={s.treeRow}
            style={{ paddingLeft: `${row.depth * 14 + 8}px` }}
            onClick={() => void toggle(row)}
            title={row.entry.path}
            role="treeitem"
            aria-expanded={opened}
          >
            {opened ? (
              <FolderOpen size={13} strokeWidth={2} />
            ) : (
              <>
                <ChevronRight size={11} strokeWidth={2} />
                <Folder size={13} strokeWidth={2} />
              </>
            )}
            <span className={s.treeName}>{row.entry.name}</span>
          </button>
        );
      })}
      {rows.length === 0 ? <EmptyPane text="这个目录是空的。" /> : null}
    </div>
  );
}

/* --- git: what the working tree looks like and where it has been --- */

function GitPane({ root }: { root: string }) {
  const [view, setView] = useState<GitView | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    if (!root) return;
    gitStatus(root)
      .then((answer) => {
        if (!cancelled) setView(answer);
      })
      .catch((failure) => {
        if (!cancelled) setError(failure instanceof Error ? failure.message : String(failure));
      });
    return () => {
      cancelled = true;
    };
  }, [root]);

  if (error) return <EmptyPane text={error} />;
  if (!view) return <EmptyPane text="正在读取 git 状态…" />;
  if (view.error) return <EmptyPane text={view.error} />;

  return (
    <div className={s.git}>
      {view.branch ? <div className={s.gitBranch}>{view.branch}</div> : null}
      <pre className={s.gitBlock}>{view.status || "工作区是干净的。"}</pre>
      {view.log ? (
        <>
          <div className={s.gitHead}>最近提交</div>
          <pre className={s.gitLog}>{view.log}</pre>
        </>
      ) : null}
    </div>
  );
}

/* --- The empty state two of the tabs answer with, for now --- */

function EmptyPane({ text }: { text: string }) {
  return <div className={s.empty}>{text}</div>;
}
