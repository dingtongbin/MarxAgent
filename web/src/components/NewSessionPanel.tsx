import { useState } from "react";
import { createSession, listSessions } from "@/api";
import { useConversation } from "@/store/conversation";
import { useWork } from "@/store/work";
import { Composer, type LaunchMode } from "./Composer";
import { ModeCapsule } from "./ModeCapsule";
import d from "./NewSessionPanel.module.css";

/**
 * The new-session tab.
 *
 * The launcher, centred in an otherwise empty tab because there is nothing to show
 * until there is a conversation. The capsule that picks the mode sits above the
 * container, and the container below it is the one the picked mode is made of — a
 * chat launcher and a coding launcher are two containers, not one container with a
 * part switched on and off, so they are mounted on separate keys and a switch takes
 * the words with it: nothing typed for a conversation belongs in a coding task, and
 * the other way round.
 */
export function NewSessionPanel() {
  // A draft like any other: choosing a directory and then closing the tab should not
  // lose the choice. It is held here rather than in a composer because a composer of
  // one mode is not the place for what the other mode needs; the directory is a
  // property of coding mode and survives a visit to chat.
  const [workdir, setWorkdir] = useState("");
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState("");
  const [mode, setMode] = useState<LaunchMode>("coding");
  // The level a coding turn from this launcher is sent under. It belongs to
  // the launcher rather than to the model, and it travels with the first turn
  // sent from here, which is the turn that runs under it.
  const [execution, setExecution] = useState("build");
  // The model this window last used, so a new conversation starts on the one already
  // in use rather than asking again. It is read through the store rather than kept
  // here so that a choice made in a conversation is the choice a new one starts on.
  const model = useWork((state) => state.model);
  const setModel = useWork((state) => state.setModel);

  // Switching the mode replaces the container: the key change unmounts the composer
  // that was there and mounts the one for the mode being switched to. The error and
  // the in-flight flag are of the tab rather than of a mode, but an error that was
  // about a chat conversation reads as nonsense under a coding form, so switching
  // clears it. A turn that was already starting carries on; it belongs to the
  // conversation it started, wherever the launcher has moved to.
  const switchMode = (next: LaunchMode) => {
    if (next === mode) return;
    setMode(next);
    setError("");
  };

  /**
   * What pressing send means here: a conversation that does not exist yet.
   *
   * The conversation is opened first and the message sent into it, rather than the
   * other way round, because a tab can only be opened over something that exists.
   * Doing it in the other order would mean a tab pointing at a conversation the
   * server has not heard of, and a window showing an empty page for a moment for
   * reasons nobody could explain.
   */
  const start = async ({ text, workdir: directory }: { text: string; workdir?: string }) => {
    if (starting) return;
    setStarting(true);
    setError("");
    try {
      const info = await createSession({
        // The directory travels only with a coding conversation. A chat turn is
        // given none even if the launcher is holding one: where the conversation
        // lives is decided by the mode, not by whatever is left in the form.
        project: mode === "coding" ? directory || undefined : undefined,
        model: model || undefined,
      });
      const work = useWork.getState();
      work.openSession(info.id);
      // The task menu is told afterwards and on its own: it is a list of what
      // exists, and a failure to refresh it should not stop somebody from talking.
      void listSessions()
        .then((sessions) => work.setSessions(sessions))
        .catch(() => {});
      await useConversation.getState().send(info.id, text, model || undefined, mode === "coding" ? execution : undefined);
    } catch (failure) {
      // Only the conversation not opening lands here. A turn that fails after that
      // has already gone to the tab, where it is shown next to the message it is
      // about.
      setError(failure instanceof Error ? failure.message : String(failure));
      setStarting(false);
    }
  };

  return (
    <div className={d.wrap}>
      <div className={d.launcher}>
        <ModeCapsule value={mode} onChange={switchMode} />
        {/* The one element that makes the composer's three parts into a single thing
            with an edge around it. The Composer returns a fragment, so without this
            its top row, field and button row are siblings in the launcher and each
            would need its own border — three lines around one control.

            The key is the mode, which is what makes the two containers two rather
            than one: switching modes remounts the composer, so no state of the form
            crosses the switch with anybody. */}
        <div className={d.card} key={mode}>
          <Composer
            variant="new"
            mode={mode}
            workdir={mode === "coding" ? workdir : ""}
            onWorkdirChange={setWorkdir}
            model={model}
            onModelChange={setModel}
            execution={execution}
            onExecutionChange={setExecution}
            onSubmit={(submit) => void start(submit)}
            running={starting}
          />
        </div>
        {error ? <div className={d.conversationError}>{error}</div> : null}
      </div>
    </div>
  );
}
