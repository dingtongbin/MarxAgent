import { memo } from "react";
import { useWork, type WorkTab } from "@/store/work";
import { NewSessionPanel } from "@/components/NewSessionPanel";
import { SessionView } from "@/components/SessionView";
import { SettingsPanel } from "@/components/SettingsPanel";

/**
 * The body of a tab.
 *
 * A tab that has no panel renders nothing rather than a notice. A tab exists to show
 * something, and a page saying "this page is not built yet" is a page that costs a
 * person a click to learn nothing they did not already know from the tab bar.
 */
function Panel({ tab }: { tab: WorkTab }) {
  switch (tab.kind) {
    case "new":
      return <NewSessionPanel />;
    case "session":
      return <SessionView sessionId={tab.id} />;
    case "settings":
      return <SettingsPanel />;
    default:
      return null;
  }
}

/**
 * Every tab, with only the active one mounted.
 *
 * The panels are not hidden with CSS; they are not rendered at all. A panel that is
 * hidden still holds its state, still runs its effects, and still listens for events,
 * so a window with six conversation tabs open would be doing six windows' worth of work
 * to show one of them. Keeping them mounted would be a deliberate choice to spend that,
 * and nothing here needs it.
 */
export const TabBody = memo(function TabBody({ activeTab }: { activeTab: string }) {
  const tab = useWork((state) => state.tabs.find((t) => t.id === activeTab));
  if (!tab) return null;
  return <Panel tab={tab} key={tab.id} />;
});
