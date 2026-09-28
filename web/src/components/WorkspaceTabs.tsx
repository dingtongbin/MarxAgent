import { Fragment, useEffect, useRef, useState, type CSSProperties, type MouseEvent } from "react";
import {
  CircleHelp,
  PanelLeft,
  PanelRight,
  Plus,
  Settings,
  WandSparkles,
  X,
} from "lucide-react";
import { projectName, sessionOf, useWork, type FixedKind, type WorkTab } from "@/store/work";
import { useUI } from "@/store/ui";
import s from "./WorkspaceTabs.module.css";

/**
 * The tab manager, and the only navigation there is.
 *
 * Left is the control that folds the task menu away. The middle is the tabs. The right
 * is the handful of switches that are not places you go but things you turn on. A
 * right-click anywhere gives the menu for what is under the pointer, including the
 * empty space, which is where somebody looks for "new".
 */
export function WorkspaceTabs() {
  const tabs = useWork((st) => st.tabs);
  const activeTab = useWork((st) => st.activeTab);
  const sessions = useWork((st) => st.sessions);
  const toggleLeft = useUI((u) => u.toggleLeft);
  const rightOpen = useUI((u) => u.rightOpen);
  const toggleRight = useUI((u) => u.toggleRight);

  const [menu, setMenu] = useState<{ x: number; y: number; tabId: string | null } | null>(null);
  const tabsRef = useRef<HTMLDivElement>(null);

  /**
   * A vertical wheel over the tab strip scrolls it sideways.
   *
   * A strip that only moves sideways has to be dragged with a horizontal gesture, and
   * nobody's first instinct on a wheel is horizontal. The approach is exponential
   * rather than applied directly because a wheel notch applied straight to scrollLeft
   * moves in visible jumps, and a strip that lurches is harder to aim at than one that
   * glides. A touchpad's own horizontal delta is passed through untouched, so a gesture
   * that is already sideways stays one-to-one.
   */
  useEffect(() => {
    const element = tabsRef.current;
    if (!element) return;
    let target = element.scrollLeft;
    let frame = 0;
    const step = () => {
      frame = 0;
      const current = element.scrollLeft;
      const delta = target - current;
      if (Math.abs(delta) < 1) {
        element.scrollLeft = target;
        return;
      }
      element.scrollLeft = current + delta * 0.25;
      frame = requestAnimationFrame(step);
    };
    const onWheel = (event: WheelEvent) => {
      // Nothing to scroll, so nothing is taken over: swallowing the wheel here would
      // stop the page scrolling for a strip that is not overflowing.
      if (element.scrollWidth <= element.clientWidth) return;
      event.preventDefault();
      const max = element.scrollWidth - element.clientWidth;
      target = Math.min(max, Math.max(0, target + event.deltaY + event.deltaX));
      if (!frame) frame = requestAnimationFrame(step);
    };
    element.addEventListener("wheel", onWheel, { passive: false });
    return () => {
      element.removeEventListener("wheel", onWheel);
      if (frame) cancelAnimationFrame(frame);
    };
  }, []);

  // The tab that is showing is brought into view when it changes, because opening a
  // tab at the far right of twenty and not seeing it is indistinguishable from the
  // click not having worked.
  useEffect(() => {
    const element = tabsRef.current;
    const active = element?.querySelector<HTMLElement>('[aria-selected="true"]');
    active?.scrollIntoView({ behavior: "smooth", inline: "nearest", block: "nearest" });
  }, [activeTab]);

  const openMenu = (event: MouseEvent, tabId: string | null) => {
    event.preventDefault();
    event.stopPropagation();
    setMenu({ x: event.clientX, y: event.clientY, tabId });
  };

  return (
    <div className={s.bar} onContextMenu={(event) => openMenu(event, null)}>
      <button className={s.ctrl} onClick={toggleLeft} title="收起/展开任务栏">
        <PanelLeft size={18} strokeWidth={2} />
      </button>

      <div className={s.tabs} ref={tabsRef}>
        {tabs.map((tab) => {
          const active = tab.id === activeTab;
          return (
            <div
              key={tab.id}
              className={`${s.tab} ${active ? s.tabOn : ""}`}
              onClick={() => useWork.getState().activateTab(tab.id)}
              onContextMenu={(event) => openMenu(event, tab.id)}
              role="tab"
              aria-selected={active}
              title={tabLabel(tab, sessions)}
            >
              <TabIcon tab={tab} state={sessionOf(tab, sessions)?.state} />
              <span className={s.label}>{tabLabel(tab, sessions)}</span>
              <button
                className={s.close}
                onClick={(event) => {
                  event.stopPropagation();
                  useWork.getState().closeTab(tab.id);
                }}
                title="关闭标签页"
              >
                <X size={13} strokeWidth={2} />
              </button>
            </div>
          );
        })}
        <button
          className={s.ctrl}
          onClick={() => useWork.getState().openFixed("new")}
          title="新建会话"
        >
          <Plus size={18} strokeWidth={2} />
        </button>
      </div>

      <div className={s.right}>
        <button
          className={s.ctrl}
          onClick={() => useWork.getState().openFixed("help")}
          title="帮助"
        >
          <CircleHelp size={18} strokeWidth={2} />
        </button>
        <button
          className={`${s.ctrl} ${tabs.find((t) => t.id === activeTab)?.kind === "settings" ? s.ctrlOn : ""}`}
          onClick={() => useWork.getState().openFixed("settings")}
          title="设置"
        >
          <Settings size={18} strokeWidth={2} />
        </button>
        <button
          className={`${s.ctrl} ${rightOpen ? s.ctrlOn : ""}`}
          onClick={toggleRight}
          title="收纳右侧栏"
        >
          <PanelRight size={18} strokeWidth={2} />
        </button>
      </div>

      {menu ? <TabMenu menu={menu} onClose={() => setMenu(null)} /> : null}
    </div>
  );
}

/**
 * What a tab is called.
 *
 * A session is named for where it lives and what was said in it, separated by a colon,
 * so a strip of twenty tabs can be told apart at a glance: the part before the colon
 * says which project, the part after says which conversation. Long names are cut with
 * an ellipsis by the stylesheet rather than here, so the width of the text is decided
 * by the width of the box.
 */
function tabLabel(tab: WorkTab, sessions: Parameters<typeof sessionOf>[1]): string {
  if (tab.kind === "settings") return "设置";
  if (tab.kind === "new") return "新建会话";
  if (tab.kind === "skills") return "技能管理";
  if (tab.kind === "help") return "帮助";
  const session = sessions.find((x) => x.id === tab.id);
  const first = session?.title?.trim();
  if (session?.project) {
    const name = projectName(session.project);
    return first ? `${name}:${first}` : name;
  }
  return first ? `任务:${first}` : "任务";
}

function TabIcon({ tab, state }: { tab: WorkTab; state?: string }) {
  switch (tab.kind) {
    case "settings":
      return <Settings size={16} strokeWidth={2} className={s.tabIcon} />;
    case "new":
      return <Plus size={16} strokeWidth={2} className={s.tabIcon} />;
    case "skills":
      return <WandSparkles size={16} strokeWidth={2} className={s.tabIcon} />;
    case "help":
      return <CircleHelp size={16} strokeWidth={2} className={s.tabIcon} />;
    default:
      return <span className={`${s.dot} ${state === "running" ? s.dotRun : s.dotIdle}`} />;
  }
}

/** The right-click menu, over a tab or over the strip itself. */
function TabMenu({ menu, onClose }: { menu: { x: number; y: number; tabId: string | null }; onClose: () => void }) {
  const tab = useWork.getState().tabs.find((t) => t.id === menu.tabId) ?? null;

  const act = (fn: () => void) => () => {
    fn();
    onClose();
  };
  const item = (key: string, label: string, fn: () => void, danger = false) => (
    <button
      key={key}
      className={`${s.menuItem} ${danger ? s.menuDanger : ""}`}
      onClick={act(fn)}
    >
      {label}
    </button>
  );

  const items = [];
  if (tab) {
    items.push(
      item("close", "关闭标签页", () => useWork.getState().closeTab(tab.id), true),
      item("others", "关闭其他标签页", () => useWork.getState().closeOthers(tab.id), true),
      item("all", "关闭全部标签页", () => useWork.getState().closeAll(), true),
    );
  } else {
    // The empty space of the strip is where somebody looks for the things that are not
    // a conversation, so the menu there offers those rather than tab commands.
    (["new", "skills", "help", "settings"] as FixedKind[]).forEach((kind) => {
      items.push(item(kind, fixedLabel(kind), () => useWork.getState().openFixed(kind)));
    });
  }

  const style: CSSProperties = {
    left: Math.min(menu.x, window.innerWidth - 190),
    top: Math.min(menu.y, window.innerHeight - 230),
  };

  return (
    <div className={s.menuOverlay} onClick={onClose} onContextMenu={(e) => e.preventDefault()}>
      <div className={s.menu} style={style}>
        {items.map((entry, index) => (
          <Fragment key={index}>{entry}</Fragment>
        ))}
      </div>
    </div>
  );
}

function fixedLabel(kind: FixedKind): string {
  switch (kind) {
    case "new":
      return "新建会话";
    case "skills":
      return "技能管理";
    case "help":
      return "帮助";
    case "settings":
      return "设置";
  }
}
