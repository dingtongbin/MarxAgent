import { useMemo, useState } from "react";
import {
  Archive,
  ArchiveRestore,
  ArrowUpDown,
  ChevronRight,
  ChevronsDown,
  ChevronsUp,
  Ellipsis,
  Plus,
  Trash2,
  WandSparkles,
} from "lucide-react";
import { projectName, useWork, type SessionInfo } from "@/store/work";
import { useUI } from "@/store/ui";
import s from "./Sidebar.module.css";

type SortKey = "recent" | "name";

type ProjectGroup = {
  path: string;
  name: string;
  sessions: SessionInfo[];
};

/**
 * The task menu.
 *
 * Three entries at the top for the things a person does rather than returns to, and
 * below them the projects, each one holding the conversations that belong to it. The
 * right edge drags.
 *
 * It is a background rather than a card: the menu is part of the canvas, and an edge
 * drawn down its right would present it as an object sitting on the window rather than
 * as the left-hand part of the window.
 */
export function Sidebar() {
  const sessions = useWork((st) => st.sessions);
  const activeTab = useWork((st) => st.activeTab);
  const openSession = useWork((st) => st.openSession);
  const openFixed = useWork((st) => st.openFixed);
  const collapsed = useUI((u) => u.leftCollapsed);
  const sidebarWidth = useUI((u) => u.sidebarWidth);

  const [sortKey, setSortKey] = useState<SortKey>("recent");
  const [sortOpen, setSortOpen] = useState(false);
  const [showArchived, setShowArchived] = useState(false);
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});
  const [rowMenu, setRowMenu] = useState<{ id: string; x: number; y: number } | null>(null);

  const groups = useMemo<ProjectGroup[]>(() => {
    const visible = sessions.filter((session) => !!session.archived === showArchived);
    const byProject = new Map<string, SessionInfo[]>();
    for (const session of visible) {
      const key = session.project ?? "";
      const list = byProject.get(key) ?? [];
      list.push(session);
      byProject.set(key, list);
    }
    const rows = [...byProject.entries()].map(([path, list]) => ({
      path,
      name: projectName(path),
      sessions: [...list].sort((a, b) =>
        (b.updated_at ?? b.created_at ?? "").localeCompare(a.updated_at ?? a.created_at ?? ""),
      ),
    }));
    rows.sort((a, b) =>
      sortKey === "name"
        ? a.name.localeCompare(b.name)
        : (b.sessions[0]?.updated_at ?? b.sessions[0]?.created_at ?? "").localeCompare(a.sessions[0]?.updated_at ?? a.sessions[0]?.created_at ?? ""),
    );
    return rows;
  }, [sessions, showArchived, sortKey]);

  // Collapsed means gone. A rail of truncated words is not a smaller menu, it is a menu
  // that cannot be read, and the control that brings it back is at the left end of the
  // tab bar, which is where a person looks for it.
  if (collapsed) return null;

  const menuSession = rowMenu ? sessions.find((x) => x.id === rowMenu.id) : null;
  const menuBusy = menuSession?.state === "running";

  const collapseAll = () => setExpanded(Object.fromEntries(groups.map((g) => [g.path, false])));
  const expandAll = () => setExpanded(Object.fromEntries(groups.map((g) => [g.path, true])));
  const isOpen = (path: string) => expanded[path] !== false;

  const onResizeStart = (event: React.PointerEvent) => {
    event.preventDefault();
    const startX = event.clientX;
    const startWidth = useUI.getState().sidebarWidth;
    const onMove = (move: PointerEvent) =>
      useUI.getState().setSidebarWidth(startWidth + move.clientX - startX);
    const onUp = () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      // The class is on the body so the cursor stays a resize cursor over the whole
      // window while the drag is happening, including over text that would otherwise
      // show a text cursor and make it look like the drag had ended.
      document.body.classList.remove("kc-resizing");
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    document.body.classList.add("kc-resizing");
  };

  return (
    <aside className={s.sidebar} style={{ width: sidebarWidth, minWidth: sidebarWidth }}>
      <div className={s.resize} onPointerDown={onResizeStart} title="拖拽调整宽度" />
      <div className={s.brand}>
        <img className={s.brandLogo} src="/logo.png" alt="" draggable={false} />
        <span>MarxAgent</span>
      </div>

      <div className={s.entries}>
        <button className={s.entry} onClick={() => openFixed("new")}>
          <Plus size={19} strokeWidth={2} />
          新建任务
        </button>
        <button className={s.entry} onClick={() => openFixed("skills")}>
          <WandSparkles size={19} strokeWidth={2} />
          技能管理
        </button>
      </div>

      <div className={s.sectionHead}>
        <span className={s.sectionTitle}>项目</span>
        <div className={s.headBtns}>
          <button className={s.headBtn} onClick={collapseAll} title="收起全部">
            <ChevronsDown size={16} strokeWidth={2} />
          </button>
          <button className={s.headBtn} onClick={expandAll} title="展开全部">
            <ChevronsUp size={16} strokeWidth={2} />
          </button>
          <div className={s.sortWrap}>
            <button
              className={`${s.headBtn} ${sortKey !== "recent" ? s.headBtnOn : ""}`}
              onClick={() => setSortOpen(!sortOpen)}
              title="排序规则"
            >
              <ArrowUpDown size={16} strokeWidth={2} />
            </button>
            {sortOpen ? (
              <>
                <div className={s.sortShield} onClick={() => setSortOpen(false)} />
                <div className={s.sortMenu}>
                  <button
                    className={`${s.sortItem} ${sortKey === "recent" ? s.sortOn : ""}`}
                    onClick={() => {
                      setSortKey("recent");
                      setSortOpen(false);
                    }}
                  >
                    按最近活动
                  </button>
                  <button
                    className={`${s.sortItem} ${sortKey === "name" ? s.sortOn : ""}`}
                    onClick={() => {
                      setSortKey("name");
                      setSortOpen(false);
                    }}
                  >
                    按名称
                  </button>
                </div>
              </>
            ) : null}
          </div>
          <button
            className={`${s.headBtn} ${showArchived ? s.headBtnOn : ""}`}
            onClick={() => setShowArchived(!showArchived)}
            title={showArchived ? "切换为展示进行中的会话" : "切换为展示归档的会话"}
          >
            {showArchived ? (
              <ArchiveRestore size={16} strokeWidth={2} />
            ) : (
              <Archive size={16} strokeWidth={2} />
            )}
          </button>
        </div>
      </div>

      <div className={s.scroller}>
        {groups.length === 0 ? (
          <div className={s.treeEmpty}>
            {showArchived ? "没有归档会话" : "暂无会话，新建任务开始"}
          </div>
        ) : null}
        {groups.map((group) => (
          <div key={group.path || "(none)"}>
            <div
              className={s.projectRow}
              role="button"
              tabIndex={0}
              aria-expanded={isOpen(group.path)}
              onClick={() => setExpanded({ ...expanded, [group.path]: !isOpen(group.path) })}
              onKeyDown={(event) => {
                if (event.key === "Enter" || event.key === " ") {
                  event.preventDefault();
                  setExpanded({ ...expanded, [group.path]: !isOpen(group.path) });
                }
              }}
            >
              <ChevronRight
                size={14}
                strokeWidth={2}
                className={`${s.chev} ${isOpen(group.path) ? s.chevOpen : ""}`}
              />
              <span
                className={s.projectName}
                title={group.path || "任务（未选择文件夹的会话）"}
              >
                {group.name}
              </span>
              <span className={s.count}>{group.sessions.length}</span>
              {/* A task with no directory has nothing to put a session inside, so the
                  button that would create one there is not drawn. */}
              {group.path ? (
                <button
                  className={s.projectPlus}
                  title={`在「${group.name}」下新建会话`}
                  onClick={(event) => {
                    event.stopPropagation();
                    openFixed("new");
                  }}
                >
                  <Plus size={15} strokeWidth={2} />
                </button>
              ) : null}
            </div>
            {isOpen(group.path) ? (
              <div className={s.sessionScroll}>
                {group.sessions.map((session) => {
                  const running = session.state === "running";
                  return (
                    <div
                      key={session.id}
                      role="button"
                      tabIndex={0}
                      className={`${s.sessionRow} ${session.id === activeTab ? s.rowOn : ""} ${showArchived ? s.rowArchived : ""}`}
                      onClick={() => openSession(session.id)}
                      onKeyDown={(event) => {
                        if (event.key === "Enter" || event.key === " ") {
                          event.preventDefault();
                          openSession(session.id);
                        }
                      }}
                      title={session.title || stateLabel(session.state)}
                      onContextMenu={(event) => {
                        event.preventDefault();
                        event.stopPropagation();
                        const rect = event.currentTarget.getBoundingClientRect();
                        setRowMenu({
                          id: session.id,
                          x: rect.left + rect.width / 2,
                          y: rect.bottom,
                        });
                      }}
                    >
                      {/* An idle row shows no mark at all. A dot on every row is a
                          column of dots, and the one that means something has to be the
                          one that is different. */}
                      {running ? (
                        <ChevronRight
                          size={14}
                          strokeWidth={2}
                          className={`${s.chev} ${s.chevRun} ${s.chevOpen}`}
                        />
                      ) : null}
                      <span className={s.rowTitle}>
                        {session.title || stateLabel(session.state)}
                      </span>
                      <span className={s.rowTime}>{hhmm(session.updated_at ?? session.created_at)}</span>
                      <button
                        className={s.rowDots}
                        title="会话操作"
                        onClick={(event) => {
                          event.stopPropagation();
                          const rect = event.currentTarget.getBoundingClientRect();
                          setRowMenu({
                            id: session.id,
                            x: rect.left + rect.width / 2,
                            y: rect.bottom,
                          });
                        }}
                      >
                        <Ellipsis size={15} strokeWidth={2} />
                      </button>
                    </div>
                  );
                })}
              </div>
            ) : null}
          </div>
        ))}
      </div>

      {rowMenu ? (
        <>
          <div
            className={s.rowShield}
            onClick={() => setRowMenu(null)}
            onContextMenu={(event) => {
              event.preventDefault();
              setRowMenu(null);
            }}
          />
          <div
            className={s.rowMenu}
            style={{
              left: Math.min(rowMenu.x, window.innerWidth - 140),
              top: Math.min(rowMenu.y + 4, window.innerHeight - 96),
            }}
          >
            {/* Deleting a running conversation is refused rather than queued. A delete
                that lands halfway through would leave a half-written session in the
                ledger, and the person who asked for it has no way to know. */}
            <button
              className={`${s.rowMenuItem} ${s.rowMenuDanger}`}
              disabled={menuBusy}
              title={menuBusy ? "运行中的会话需先中断" : undefined}
              onClick={() => setRowMenu(null)}
            >
              <Trash2 size={14} strokeWidth={2} />
              删除
            </button>
          </div>
        </>
      ) : null}
    </aside>
  );
}

/** The wall clock time, which is what a person reading a list of today's work wants. */
function hhmm(iso?: string): string {
  if (!iso) return "";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return "";
  return `${String(date.getHours()).padStart(2, "0")}:${String(date.getMinutes()).padStart(2, "0")}`;
}

/** What a session with nothing said in it is called. */
function stateLabel(state: string): string {
  switch (state) {
    case "idle":
      return "空闲";
    case "running":
      return "运行中";
    case "checkpointed":
      return "已保存";
    case "degraded":
      return "降级";
    default:
      return state;
  }
}
