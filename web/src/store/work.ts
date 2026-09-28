import { create } from "zustand";
import {
  getWorkspace,
  putWorkspace,
  type SessionInfo,
  type SessionState,
  type TabTree,
} from "@/api";

export type { SessionInfo, SessionState };

/**
 * The sessions and the tabs over them.
 *
 * A session is a conversation. A tab is a way of looking at one, and the difference
 * matters: closing a tab must not close a conversation, and a conversation can be
 * closed in the task menu and still be open in a tab. Anything that treated the two
 * as the same thing would lose work the moment somebody tidied up.
 *
 * The strip is kept on the server, not here. A tab tree that lives in one window's
 * memory is gone the moment that window is reloaded, and reloading a page is
 * something people do constantly — the strip coming back is the difference between
 * a window and a tab that forgot what it was showing.
 *
 * Every change is written back, a moment after the last one. The wait is not for
 * the server: it is so that dragging a tab strip past five tabs is one request
 * rather than five.
 */

/** What a session is doing. The states are the ones the audit ledger can actually be
 *  in, so the interface never has to invent a fifth one. */

/** The tabs that are not a session: the places a person goes rather than returns to. */
export type FixedKind = "new" | "settings" | "help" | "skills";

export type WorkTab = {
  id: string;
  kind: "session" | FixedKind;
};

type WorkState = {
  tabs: WorkTab[];
  activeTab: string;
  sessions: SessionInfo[];
  /**
   * Whether the strip has been read from the server yet.
   *
   * It is here so that a change made in the first moments of a window is not
   * written back over the strip the server is about to send. A window that wrote
   * its one default tab before the read came back would erase whatever the last
   * visit left open.
   */
  loaded: boolean;
  /**
   * The model this window is about to use, in the configuration's own
   * provider:model spelling. Empty means nothing has been chosen yet, which is a
   * different thing from one that was chosen and then cleared.
   */
  model: string;
  /**
   * Which settings section to open on.
   *
   * It is in the store rather than in the settings panel because the model picker
   * asks for the model section from outside that panel: the button is in a
   * composer, and reaching across to a panel that may not be mounted is not
   * something a control should have to do.
   */
  settingsSection: string;

  activateTab: (id: string) => void;
  openSession: (id: string) => void;
  openFixed: (kind: FixedKind) => void;
  closeTab: (id: string) => void;
  closeOthers: (id: string) => void;
  closeAll: () => void;
  setSessions: (sessions: SessionInfo[]) => void;
  /**
   * Put one conversation's own account of itself into the list.
   *
   * A conversation tells the windows watching it when its state or its title has
   * changed, because a title is not known until the first thing said in it and nobody
   * is going to reload a list to find out. An unknown identifier is added rather than
   * dropped: this is how a conversation reaches a window that opened it.
   */
  upsertSession: (info: SessionInfo) => void;
  setSessionState: (id: string, state: SessionState) => void;
  setModel: (model: string) => void;
  setSettingsSection: (section: string) => void;
  /** Read this window's strip from the server, once. */
  load: () => Promise<void>;
};

/** How long the last change has to be the last change before it is written. */
const writeDelay = 400;

const lastModelKey = "marxagent.lastModel";

/**
 * The model this window last used.
 *
 * Read the way the theme and the sidebar width are read, and for the same reason: it
 * is a preference, it is worthless to nobody, and losing it costs a person a choice
 * they have already made once. A value that is not a model specification is ignored
 * rather than repaired, because a window offering a model that is not there is worse
 * than a window offering the choice again.
 */
function readLastModel(): string {
  try {
    const raw = window.localStorage.getItem(lastModelKey);
    if (!raw) return "";
    return raw.includes(":") ? raw : "";
  } catch {
    // Storage can be refused, and a window with no remembered model still lets the
    // model be chosen.
    return "";
  }
}

export const useWork = create<WorkState>((set, get) => {
  // One timer for the whole store, because a strip is one thing rather than one
  // thing per tab: two timers would write the strip twice and could write the older
  // of the two states last.
  let writeTimer: number | undefined;

  const publish = () => {
    if (!get().loaded) return;
    window.clearTimeout(writeTimer);
    writeTimer = window.setTimeout(() => {
      const current = get();
      const tree: TabTree = {
        tabs: current.tabs.map((tab) => ({ id: tab.id, kind: tab.kind })),
        active: current.activeTab,
      };
      void putWorkspace(tree)
        .then((stored) => {
          // What was kept, not what was sent: the store drops a duplicate and
          // corrects a strip whose active tab is not in it, and a window showing
          // what it sent would keep showing the duplicate until the next reload.
          set({
            tabs: stored.tabs.map((tab) => ({ id: tab.id, kind: tab.kind as WorkTab["kind"] })),
            activeTab: stored.active ?? stored.tabs[0]?.id ?? "new",
          });
        })
        .catch(() => {
          // A strip that could not be written is left as it is on screen. Saying so
          // would be better, and the tab strip is not where a person looks for it.
        });
    }, writeDelay);
  };

  return {
    // A window with no tab has nothing to show and no way to get anything back, so
    // the new-task tab is there from the first paint rather than appearing when
    // clicked. It is replaced by whatever the server has a moment later.
    tabs: [{ id: "new", kind: "new" }],
    activeTab: "new",
    sessions: [],
    loaded: false,
    // The model this window last used, so a new conversation starts on the one the
    // person is already using rather than making them choose again every time. It is
    // a preference and it is kept the way the theme and the width are kept; a
    // conversation's own model is not here, because that belongs to the conversation
    // and is written down with it on the server.
    model: readLastModel(),
    settingsSection: "general",

    load: async () => {
      if (get().loaded) return;
      try {
        const tree = await getWorkspace();
        // A strip with nothing in it is not a strip; the server never stores one,
        // and a window with no tab has no way to get anything back.
        const tabs = tree.tabs.length > 0 ? tree.tabs : [{ id: "new", kind: "new" as const }];
        set({
          tabs: tabs.map((tab) => ({ id: tab.id, kind: tab.kind as WorkTab["kind"] })),
          activeTab: tree.active ?? tabs[0].id,
          loaded: true,
        });
      } catch {
        // The window still works: the strip is in memory, and the next change will
        // try to write it again.
        set({ loaded: true });
      }
    },

    activateTab: (id) => {
      set({ activeTab: id });
      publish();
    },

    openSession: (id) => {
      const state = get();
      if (state.tabs.some((tab) => tab.id === id)) {
        set({ activeTab: id });
      } else {
        set({ tabs: [...state.tabs, { id, kind: "session" }], activeTab: id });
      }
      publish();
    },

    openFixed: (kind) => {
      const state = get();
      const existing = state.tabs.find((tab) => tab.kind === kind);
      // A singleton tab that is already open is activated rather than duplicated, and
      // the click that would have opened a second copy is the click that brought the
      // first one forward.
      if (existing) {
        set({ activeTab: existing.id });
        publish();
        return;
      }
      set({ tabs: [...state.tabs, { id: kind, kind }], activeTab: kind });
      publish();
    },

    closeTab: (id) => {
      const state = get();
      const index = state.tabs.findIndex((tab) => tab.id === id);
      if (index < 0) return;
      const tabs = state.tabs.filter((tab) => tab.id !== id);
      // The window must never be left with nothing to show. If the tab that was
      // showing is the one being closed, the neighbour to the left takes over, which
      // is the tab the eye was already next to.
      if (tabs.length === 0) {
        set({ tabs: [{ id: "new", kind: "new" }], activeTab: "new" });
        publish();
        return;
      }
      const activeTab =
        state.activeTab === id ? (tabs[Math.max(0, index - 1)]?.id ?? tabs[0].id) : state.activeTab;
      set({ tabs, activeTab });
      publish();
    },

    closeOthers: (id) => {
      set({ tabs: get().tabs.filter((tab) => tab.id === id), activeTab: id });
      publish();
    },

    closeAll: () => {
      set({ tabs: [{ id: "new", kind: "new" }], activeTab: "new" });
      publish();
    },

    setSessions: (sessions) => set({ sessions }),

    upsertSession: (info) =>
      set((current) => {
        const known = current.sessions.some((session) => session.id === info.id);
        return {
          sessions: known
            ? current.sessions.map((session) => (session.id === info.id ? info : session))
            : [...current.sessions, info],
        };
      }),

    setModel: (model) => {
      set({ model });
      // Written on the choice rather than on the turn: somebody who picks a model and
      // then closes the tab meant to use it next time too, and the pick is the moment
      // that was expressed.
      try {
        window.localStorage.setItem(lastModelKey, model);
      } catch {
        // Nothing to do; the choice still holds for this window.
      }
    },

    setSettingsSection: (section) => set({ settingsSection: section }),

    setSessionState: (id, state) =>
      set((current) => ({
        sessions: current.sessions.map((session) =>
          session.id === id ? { ...session, state } : session,
        ),
      })),
  };
});

/** The session a tab is showing, if it is showing one. */
export function sessionOf(tab: WorkTab | undefined, sessions: SessionInfo[]): SessionInfo | undefined {
  if (!tab || tab.kind !== "session") return undefined;
  return sessions.find((session) => session.id === tab.id);
}

/**
 * The short name of a project, which is its last path segment.
 *
 * A path is what the filesystem calls it and a name is what a person calls it, and a
 * list of full paths in a 232 pixel column is a list of the same last segment with the
 * interesting part scrolled off to the left. Windows paths are split on both separators
 * because a path can contain either regardless of where it came from.
 */
export function projectName(path: string | undefined): string {
  if (!path) return "";
  const parts = path.split(/[\\/]+/).filter(Boolean);
  return parts[parts.length - 1] ?? path;
}
