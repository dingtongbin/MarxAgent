import { useEffect, useRef, useState } from "react";
import {
  Check,
  ChevronDown,
  ChevronRight,
  FolderGit2,
  Paperclip,
  Plus,
  Settings,
  X,
  ArrowUp,
} from "lucide-react";
import { getConfig } from "@/api";
import { DirPicker } from "./DirPicker";
import { ExecutionPicker } from "./ExecutionPicker";
import { useWork } from "@/store/work";
import d from "./NewSessionPanel.module.css";

/** What a file waiting to go with the message is called. */
type Pending = { name: string; size: number };

/**
 * The two modes a launcher can be set to.
 *
 * They are exported because the launcher names them too: the capsule above the
 * composer and the composer itself have to agree on what the two words are, and
 * two files each spelling their own pair is a spelling that can drift.
 */
export type LaunchMode = "chat" | "coding";

/**
 * The composer.
 *
 * The shape is the one the stylesheet was written for: the directory along the top
 * edge, the field in the middle, and a row of controls under it. The controls that are
 * here are the ones that work with nothing from a server — the directory, the text, the
 * files, and sending. The context ring, the model chip, the thinking level and the undo
 * stack all read numbers this window has not been told, so they are not drawn: a row of
 * controls that report nothing is worse than a row of four that do something.
 *
 * The mode is what decides what the top edge is and what sending requires, and the two
 * modes are deliberately not one form with some fields hidden. A conversation and a
 * coding task are different things being asked of the same model: one has no directory
 * at all and the other cannot start without one. A form that toggles a field on and off
 * is one form pretending to be two; this is two forms that happen to share a field row
 * and a send control, and they behave differently from the first keystroke.
 */
export function Composer({
  variant,
  mode = "coding",
  workdir,
  onWorkdirChange,
  onSubmit,
  running,
  onInterrupt,
  model,
  onModelChange,
  execution,
  onExecutionChange,
}: {
  /** "new" is the launcher in an empty tab; "session" is the foot of a conversation. */
  variant: "new" | "session";
  /**
   * Which mode this composer is. Only the launcher chooses one; a conversation is
   * already the mode it was opened in.
   */
  mode?: LaunchMode;
  workdir: string;
  /** Only the launcher can change it; a conversation is opened in a project. */
  onWorkdirChange?: (path: string) => void;
  onSubmit: (submit: { text: string; workdir?: string; execution?: string }) => void;
  running?: boolean;
  onInterrupt?: () => void;
  /**
   * The model in force here, and how to change it.
   *
   * Passed in rather than read from the window because a conversation and a launcher
   * are not the same place: a conversation that has been talking to one model shows
   * that model and changes it for itself, while a new conversation starts on whatever
   * this window last used. Reading one global value for both would show a conversation
   * a model it is not using.
   */
  model?: string;
  onModelChange?: (model: string) => void;
  /**
   * The execution level a coding turn is sent under, and how to change it.
   *
   * It is the caller's value because the level is the conversation's and the
   * launcher's, not the composer's: a conversation carries what its last turn
   * ran at, a launcher holds what this window is about to send. It is shown and
   * changed only where it means anything, which is a coding composer; a chat
   * one has no reach to name.
   */
  execution?: string;
  onExecutionChange?: (execution: string) => void;
}) {
  const [text, setText] = useState("");
  const [dirOpen, setDirOpen] = useState(false);
  const [plusOpen, setPlusOpen] = useState(false);
  const [pending, setPending] = useState<Pending[]>([]);
  const [pickerOpen, setPickerOpen] = useState(false);
  // The projects this window has seen, from the sessions that belong to them. Read
  // rather than remembered, so a project that has no sessions left stops being offered
  // on its own.
  const sessions = useWork((state) => state.sessions);
  const known = Array.from(new Set(sessions.map((item) => item.project).filter(Boolean))) as string[];
  const inputRef = useRef<HTMLTextAreaElement>(null);

  // The field grows with what is in it, up to the limit the stylesheet sets, and stops
  // there. Sizing it to the content means the card does not move while somebody is part
  // way through a sentence.
  useEffect(() => {
    const element = inputRef.current;
    if (!element) return;
    element.style.height = "auto";
    element.style.height = `${element.scrollHeight}px`;
  }, [text]);

  // A launcher set to chat is a composer with no directory in it at all: the top row
  // is not hidden, it does not exist, and the field starts where the card starts.
  // The launcher still holds a directory underneath it, because switching to coding
  // is not a reason to lose a choice that was made; it is just not this form's input.
  const dirRow = variant === "new" ? mode === "coding" : workdir.trim().length > 0;
  // Coding cannot start without a directory — there is no project to be audited in.
  // Chat has nothing to do with one, so its send control asks for nothing but words.
  const needsProject = variant === "new" && mode === "coding";
  const hasProject = workdir.trim().length > 0;
  const canSend = text.trim().length > 0 && !running && (!needsProject || hasProject);

  const send = () => {
    if (!canSend) return;
    // A chat turn is sent without a directory even when the launcher is holding
    // one: the mode is what decides where the conversation lives, and a chat
    // conversation lives in the application data, not in a project.
    onSubmit({
      text: text.trim(),
      workdir: needsProject ? workdir.trim() || undefined : undefined,
      // The level travels with the turn that runs under it, so the choice
      // made before the send is the one that turn is bound by.
      execution: needsProject ? execution || undefined : undefined,
    });
    setText("");
    setPending([]);
  };

  const addFiles = (list: FileList | null) => {
    if (!list) return;
    setPending((previous) => [
      ...previous,
      ...[...list].map((file) => ({ name: file.name, size: file.size })),
    ]);
  };

  return (
    <>
      {/* The top edge, and in coding mode it carries the directory. It is where the
          work happens, so it is a field of this form rather than something above it.
          Chat mode has no top edge: a conversation that lives in the application data
          has no directory to show, and a row that says nothing is a row that should
          not be there.

          Inside a conversation the row is the one field that is not a field. A coding
          conversation belongs to the project it was opened with, and the chips below
          can reach files there; moving a conversation onto another project's data is a
          different thing from a message, so it is shown and not offered. */}
      {dirRow && variant === "new" ? (
        <div className={d.topRow}>
        <div className={d.anchor}>
          <button
            className={`${d.chip} ${dirOpen ? d.chipOn : ""} ${workdir ? "" : d.chipNeeded}`}
            onClick={() => setDirOpen(!dirOpen)}
            aria-haspopup="dialog"
            aria-expanded={dirOpen}
            title={workdir ? workdir : "选一个项目目录才能开始"}
          >
            <FolderGit2 size={15} strokeWidth={2} />
            <span className={d.chipText}>{workdir ? lastSegment(workdir) : "选择项目目录"}</span>
            <ChevronDown size={14} strokeWidth={2} className={d.chipChev} />
          </button>
          {dirOpen ? (
            <>
              <div className={d.shield} onClick={() => setDirOpen(false)} />
              <div className={`${d.upMenu} ${d.upMenuLeft}`} role="menu">
                <div className={d.menuSection}>打开过的项目</div>
                {known.length === 0 ? null : (
                  known.map((path) => (
                    <button
                      key={path}
                      role="menuitemradio"
                      aria-checked={path === workdir}
                      title={path}
                      className={`${d.menuItem} ${path === workdir ? d.menuOn : ""}`}
                      onClick={() => {
                        onWorkdirChange?.(path);
                        setDirOpen(false);
                      }}
                    >
                      {lastSegment(path)}
                      <span className={d.menuSub}>{path}</span>
                    </button>
                  ))
                )}
                <button
                  className={d.pickRow}
                  onClick={() => {
                    setDirOpen(false);
                    setPickerOpen(true);
                  }}
                >
                  打开文件夹…
                </button>
              </div>
            </>
          ) : null}
        </div>
        </div>
      ) : dirRow && workdir ? (
        <div className={d.topRow}>
          <span className={`${d.chip} ${d.chipOff}`} title={`这个会话在 ${workdir}`}>
            <FolderGit2 size={15} strokeWidth={2} />
            <span className={d.chipText}>{lastSegment(workdir)}</span>
          </span>
        </div>
      ) : null}

      <textarea
        ref={inputRef}
        className={d.input}
        value={text}
        rows={2}
        // The field says what this composer is for rather than accepting words that
        // cannot be sent. Coding names its one requirement; chat has no requirement
        // and so says nothing about one.
        placeholder={
          needsProject
            ? hasProject
              ? "描述编程任务。我可以先读代码，再做最小的修改，并在项目内构建验证。"
              : "先选一个项目目录，再描述任务。"
            : "有什么想聊的？"
        }
        onChange={(event) => setText(event.target.value)}
        onKeyDown={(event) => {
          // Enter sends and Shift+Enter is a newline. Somebody typing a list of steps
          // should not have it sent part way through.
          if (event.key === "Enter" && !event.shiftKey) {
            event.preventDefault();
            send();
          }
        }}
      />

      {/* The row under the field. What can be added on the left, what happens to it on
          the right. The execution level sits to the right of the attachments control,
          because it is read after the message is made: the attachments are what goes
          with the words, and the level is what those words may reach. It is shown
          only where it means anything, which is a coding composer. */}
      <div className={d.btnRow}>
        <div className={d.left}>
          <div className={d.anchor}>
            <button
              className={d.iconBtn}
              onClick={() => setPlusOpen(!plusOpen)}
              title="附件"
              aria-haspopup="menu"
              aria-expanded={plusOpen}
            >
              <Plus size={16} strokeWidth={2} />
            </button>
            {plusOpen ? (
              <>
                <div className={d.shield} onClick={() => setPlusOpen(false)} />
                <div className={`${d.upMenu} ${d.upMenuLeft} ${d.plusPanel}`} role="menu">
                  <div className={d.menuSection}>
                    <Paperclip size={13} strokeWidth={2} /> 附件
                  </div>
                  {/* A file is chosen here and read by the browser, so this works with
                      nothing from a server. Sending it anywhere does not. */}
                  <label className={d.pickRow}>
                    <input
                      type="file"
                      multiple
                      style={{ display: "none" }}
                      onChange={(event) => {
                        addFiles(event.target.files);
                        event.currentTarget.value = "";
                      }}
                    />
                    选择文件…
                  </label>
                  {pending.length > 0 ? (
                    <div className={d.fileChips}>
                      {pending.map((file, index) => (
                        <span key={`${file.name}-${index}`} className={d.fileChip} title={file.name}>
                          {file.name}
                          <button
                            className={d.fileChipX}
                            onClick={() =>
                              setPending((previous) => previous.filter((_, i) => i !== index))
                            }
                            title="移除"
                          >
                            <X size={11} strokeWidth={2} />
                          </button>
                        </span>
                      ))}
                    </div>
                  ) : null}
                </div>
              </>
            ) : null}
          </div>

          {needsProject ? (
            <ExecutionPicker
              value={execution ?? "build"}
              onChange={(level) => onExecutionChange?.(level)}
            />
          ) : null}
        </div>

        <div className={d.right}>
          <ModelPicker
            value={model ?? useWork.getState().model}
            onChange={(next) => onModelChange?.(next)}
          />

          {/* A running turn gets a stop in the same seat as the send control. A control
              that changes what it does but not where it is can be pressed without
              looking. */}
          {running ? (
            <button className={d.stop} onClick={onInterrupt} title="中断">
              <span className={d.stopBox} />
            </button>
          ) : (
            <button
              className={d.send}
              onClick={send}
              disabled={!canSend}
              title={
                needsProject && !hasProject
                  ? "先选一个项目目录"
                  : variant === "new"
                    ? "开始会话"
                    : "发送"
              }
            >
              <ArrowUp size={17} strokeWidth={2} />
            </button>
          )}
        </div>
      </div>

      <DirPicker
        open={pickerOpen}
        onCancel={() => setPickerOpen(false)}
        onPick={(name) => {
          setPickerOpen(false);
          onWorkdirChange?.(name);
        }}
      />
    </>
  );
}

/**
 * Which model this turn goes to.
 *
 * It sits beside the send control rather than above the field because a model is a
 * property of the turn, and the turn is what the send button starts. A person
 * choosing a model is choosing where the words they are about to type are going,
 * and that is a decision made at the moment of sending.
 *
 * The list is what the configuration says this machine has, which is read from the
 * server rather than remembered. A remembered list would go on offering a model
 * whose key was deleted, and the failure would appear as a conversation that
 * refuses to start.
 *
 * The settings entry is the last row because it is the thing somebody reaches for
 * when the list is wrong or empty — which is to say, when they have just added a
 * provider and want to see whether it worked.
 */
function ModelPicker({
  value,
  onChange,
}: {
  /** The model in force where this composer is: a conversation's own, or the window's. */
  value: string;
  onChange: (model: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [providers, setProviders] = useState<
    { name: string; models: { spec: string; id: string }[] }[]
  >([]);
  const [defaultModel, setDefaultModel] = useState("");
  const [missing, setMissing] = useState(false);
  // Which supplier's models are open beside the list. It is a supplier and not a
  // path, because the list is the suppliers and the second level is one of them.
  const [opened, setOpened] = useState<string | null>(null);
  const anchor = useRef<HTMLDivElement>(null);
  const model = value;
  const setModel = onChange;

  // Read once for the life of the window rather than every time the menu opens: the
  // list changes when somebody edits the configuration, and a list that reappears
  // from scratch every time is a list that flickers.
  useEffect(() => {
    let cancelled = false;
    getConfig()
      .then((config) => {
        if (cancelled) return;
        setDefaultModel(config.model.default ?? "");
        setProviders(
          Object.entries(config.model.providers)
            .filter(([, provider]) => provider.enabled)
            .map(([name, provider]) => ({
              name,
              models: (provider.models ?? [])
                .filter((entry) => !entry.disabled)
                .map((entry) => ({ spec: `${name}:${entry.id}`, id: entry.id })),
            }))
            // A supplier with nothing in it is not in the list: opening it would
            // show an empty box beside the list, which is a dead end rather than a
            // choice. Somebody who has just added one goes straight to the settings
            // instead, which is what the last row is for.
            .filter((provider) => provider.models.length > 0)
            .sort((left, right) => left.name.localeCompare(right.name)),
        );
      })
      .catch(() => {
        if (!cancelled) setMissing(true);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // The menu closes when the pointer goes elsewhere, the window loses focus, or the
  // Escape key is pressed. The last one is here rather than left to the browser
  // because a menu that a person opened with a key should close with one.
  useEffect(() => {
    if (!open) return;
    const dismiss = (event: MouseEvent) => {
      if (anchor.current?.contains(event.target as Node)) return;
      setOpen(false);
      setOpened(null);
    };
    const abandon = () => {
      setOpen(false);
      setOpened(null);
    };
    const escape = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      if (opened !== null) {
        // The first Escape closes the models and leaves the suppliers, because a
        // person who is looking at the wrong supplier does not mean to close the
        // whole thing.
        setOpened(null);
        return;
      }
      setOpen(false);
    };
    document.addEventListener("mousedown", dismiss);
    window.addEventListener("blur", abandon);
    window.addEventListener("keydown", escape);
    return () => {
      document.removeEventListener("mousedown", dismiss);
      window.removeEventListener("blur", abandon);
      window.removeEventListener("keydown", escape);
    };
  }, [open, opened]);

  // What the button reads: the model that is in force, the configured default when
  // nothing was chosen, and a plain statement when there is neither. It never says
  // "default" as a fading hint — a choice that is not visible is a choice nobody
  // knows they made.
  //
  // It reads as supplier and model rather than as the model alone. Two suppliers can
  // offer a model of the same name, and which one answers is the whole of what
  // somebody is choosing here; a chip reading `deepseek-flash` cannot tell them which,
  // and neither can a chip reading `deepseek/deepseek-flash` be shortened to fit.
  const chosen = model || defaultModel;
  const label = chosen ? spelled(chosen) : missing ? "未配置模型" : "选择模型";

  return (
    <div className={d.anchor} ref={anchor}>
      <button
        className={`${d.chip} ${d.modelChip} ${chosen ? "" : d.chipMuted}`}
        onClick={() => {
          setOpen(!open);
          setOpened(null);
        }}
        aria-haspopup="menu"
        aria-expanded={open}
        title={chosen ? `模型：${chosen}` : "选择这个会话用哪个模型"}
      >
        <span className={d.chipText}>{label}</span>
        <ChevronDown size={14} strokeWidth={2} className={d.chipChev} />
      </button>

      {open ? (
        <>
          <div className={d.shield} onClick={() => setOpen(false)} />
          <div className={`${d.upMenu} ${d.upMenuRight} ${d.upMenuCascade} ${d.modelMenu}`} role="menu">
            {providers.length === 0 ? (
              <div className={d.modelEmpty}>
                {missing ? "读不到配置。" : "还没有可用模型，在下面添加一个供应商并从端点拉取。"}
              </div>
            ) : (
              providers.map((provider) => (
                <div key={provider.name} className={d.cascadeRow}>
                  <button
                    role="menuitem"
                    aria-haspopup="menu"
                    aria-expanded={opened === provider.name}
                    className={`${d.cascadeItem} ${
                      opened === provider.name ? d.cascadeItemOpen : ""
                    }`}
                    onClick={() => setOpened(provider.name)}
                    onMouseEnter={() => setOpened(provider.name)}
                  >
                    <span className={d.cascadeName}>{provider.name}</span>
                    <ChevronRight size={15} strokeWidth={2} className={d.cascadeChev} />
                  </button>

                  {opened === provider.name ? (
                    <div className={`${d.sideMenu} ${d.modelMenu}`} role="menu">
                      {provider.models.map((entry) => (
                        <button
                          key={entry.spec}
                          role="menuitemradio"
                          aria-checked={entry.spec === chosen}
                          className={`${d.modelItem} ${
                            entry.spec === chosen ? d.modelItemOn : ""
                          }`}
                          onClick={() => {
                            setModel(entry.spec);
                            setOpen(false);
                            setOpened(null);
                          }}
                        >
                          <span className={d.modelName}>{entry.id}</span>
                          {entry.spec === chosen ? (
                            <Check size={14} strokeWidth={2} className={d.modelCheck} />
                          ) : null}
                        </button>
                      ))}
                    </div>
                  ) : null}
                </div>
              ))
            )}
            <div className={d.modelDivider} />
            <button
              role="menuitem"
              className={d.modelSettings}
              onClick={() => {
                setOpen(false);
                setOpened(null);
                // The settings tab, on the section that is about the model, because
                // a person who pressed this has a model to fix.
                useWork.getState().setSettingsSection("model");
                useWork.getState().openFixed("settings");
              }}
            >
              <Settings size={15} strokeWidth={2} />
              模型设置
            </button>
          </div>
        </>
      ) : null}
    </div>
  );
}

/**
 * A model specification as a person reads it: the supplier, then the model.
 *
 * The specification itself is written `deepseek:deepseek-flash`, and a colon is a
 * separator in half a dozen other places on a machine. Here it is a slash, because
 * this is the one place the two things are being shown side by side rather than
 * addressed.
 *
 * A specification with no supplier in it is shown as it arrived rather than being
 * given a leading slash.
 */
function spelled(spec: string): string {
  const at = spec.indexOf(":");
  if (at <= 0) return spec;
  return `${spec.slice(0, at)}/${spec.slice(at + 1)}`;
}

/**
 * The last segment of a path, which is what a person calls a directory.
 *
 * Both separators are split on because a path can contain either regardless of where it
 * was typed, and a control reading `dir\projects\marxagent` is not one anybody can scan.
 */
function lastSegment(path: string): string {
  const parts = path.split(/[\\/]+/).filter(Boolean);
  return parts[parts.length - 1] ?? path;
}
