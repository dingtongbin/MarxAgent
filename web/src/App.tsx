import { useEffect } from "react";
import { listSessions } from "@/api";
import { applyTheme, useUI } from "@/store/ui";
import { useConversation } from "@/store/conversation";
import { useWork } from "@/store/work";
import { Sidebar } from "@/components/Sidebar";
import { WorkspaceTabs } from "@/components/WorkspaceTabs";
import { TabBody } from "@/panels";
import s from "./App.module.css";

/**
 * The window.
 *
 * The task menu on the left, the tab bar across the top of everything else, and the
 * work area inside. The tab bar belongs to the work area rather than to the window, so
 * folding the menu away leaves the tabs exactly where they were instead of sliding them
 * sideways under a person who is reading them.
 */
export default function App() {
  const theme = useUI((state) => state.theme);
  const activeTab = useWork((state) => state.activeTab);
  const tabs = useWork((state) => state.tabs);

  // The window asks the server what it had open, and what conversations exist, once.
  // Both are the same question asked of a machine that outlives the page: which is why
  // a reload comes back to the tabs and the conversations rather than to nothing.
  useEffect(() => {
    void useWork.getState().load();
    void listSessions()
      .then((sessions) => useWork.getState().setSessions(sessions))
      .catch(() => {
        // The task menu shows nothing rather than an error. A menu that says it could
        // not reach the server is a menu about the server, in a window about work.
      });
  }, []);

  // The theme is put on the document here as well as in the document's own early
  // script. That script runs before anything is painted and cannot know what a person
  // chose mid-session; this one runs when they change it. Same value, same place.
  useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  // Following the system theme is a subscription rather than a read, because a person
  // who has not chosen a theme expects the window to change with the machine and not to
  // need reopening.
  useEffect(() => {
    if (theme !== "system") return;
    const query = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => applyTheme("system");
    query.addEventListener("change", onChange);
    return () => query.removeEventListener("change", onChange);
  }, [theme]);

  // A stream belongs to a conversation a tab is pointed at, and it outlives the panel
  // showing that tab — a turn keeps answering while the tab is in the background, and
  // closing the stream on every switch would throw away the part that arrives while
  // this window is looking at something else. What does end it is the last tab that
  // pointed at that conversation being closed, at which point nobody is left to hand
  // the events to and the connection is only cost.
  useEffect(() => {
    const pointed = new Set(
      tabs.filter((tab) => tab.kind === "session").map((tab) => tab.id),
    );
    for (const id of Object.keys(useConversation.getState().watches)) {
      if (!pointed.has(id)) useConversation.getState().forget(id);
    }
  }, [tabs]);

  // A keyboard shortcut rather than a hidden control, because closing a tab with a
  // pointer means moving the pointer to a target that is a few pixels wide.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      // Ctrl+W is the browser's own close-tab, and taking it would leave a person with
      // no way to close the window at all. Anything else is fair game.
      if (event.key.toLowerCase() !== "w" || !(event.ctrlKey || event.metaKey)) return;
      if (event.altKey || event.shiftKey) return;
      event.preventDefault();
      useWork.getState().closeTab(useWork.getState().activeTab);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // The tab bar and the work area are siblings of the task menu, placed on the window's
  // grid rather than gathered into a box that exists only to hold them.
  return (
    <div className={s.app}>
      <Sidebar />
      <WorkspaceTabs />
      <div className={s.body}>
        <TabBody activeTab={activeTab} />
      </div>
    </div>
  );
}
