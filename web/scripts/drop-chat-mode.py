"""Take chat mode out of the interface.

Only work mode exists here for now, so there is nothing to switch between. The mode
control goes rather than being left on screen with one option: a toggle with a single
entry is a control that cannot do anything, and a person who presses it learns that the
window is not finished rather than learning anything about their work.

The mode still travels with a submit, because the server has two modes and will be told
which one this is. It is written down once here rather than passed in, so that when
chat mode comes back it is one line and one control rather than a decision to be
rediscovered.
"""

import io
import os
import re

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")

# --- the control itself, and the styles only it used ---------------------------
for name in ("ModeToggle.tsx", "ModeToggle.module.css"):
    full = os.path.join(SRC, "components", name)
    if os.path.exists(full):
        os.remove(full)
        print(f"removed components/{name}")

# --- the composer no longer takes a mode ----------------------------------------
path = os.path.join(SRC, "components", "Composer.tsx")
with io.open(path, encoding="utf-8") as handle:
    text = handle.read()

text = text.replace('import type { Mode } from "./ModeToggle";\n', "")
text = text.replace("  mode,\n  onSubmit,", "  onSubmit,")
text = text.replace("  /** The mode, chosen in the row above this container. */\n  mode: Mode;\n", "")
text = text.replace("  session?: { mode?: Mode; model?: string; tokens?: number; window?: number };",
                    "  session?: { model?: string; tokens?: number; window?: number };")
text = text.replace("onSubmit({ text: text.trim(), mode, workdir: workdir.trim() || undefined });",
                    "onSubmit({ text: text.trim(), workdir: workdir.trim() || undefined });")
text = text.replace("export type ComposerSubmit = {\n  text: string;\n  mode: Mode;\n  workdir?: string;\n};",
                    """export type ComposerSubmit = {
  text: string;
  /**
   * Work mode, always, because it is the only one that exists yet. It is written here
   * rather than being asked for, so that a window with no mode control cannot send a
   * request that says nothing about the mode it is in.
   */
  mode: "work";
  workdir?: string;
};""")
text = text.replace("onSubmit({ text: text.trim(), workdir: workdir.trim() || undefined });",
                    'onSubmit({ text: text.trim(), mode: "work", workdir: workdir.trim() || undefined });')

# The prose that described a control which is no longer here.
text = re.sub(
    r" \* `new` is the launcher in the middle of an empty tab.*?\n \*/\n",
    " * `new` is the launcher in the middle of an empty tab, where the directory is a\n"
    " * thing being chosen. `session` is the foot of a conversation, where it was\n"
    " * settled when the session was created.\n"
    " *\n"
    " * Both carry the same row of controls under the input. That row is the point of the\n"
    " * component: the input is the largest thing and the row is everything that can be\n"
    " * done to what is in it.\n"
    " */\n",
    text,
    count=1,
    flags=re.S,
)
text = re.sub(
    r"       \* The mode and the directory both live here.*?\n      <div className=\{d\.topRow\}>\n",
    "       * The top edge of the container, and it carries one thing: the directory. It\n"
    "       * is a field of the form — where the work happens — so it belongs on the form\n"
    "       * rather than above it.\n"
    "      <div className={d.topRow}>\n",
    text,
    count=1,
    flags=re.S,
)
text = text.replace(
    "        {/* One control on this side now. The mode was here as well at one\n"
    "            point, and having the same control in two places meant one of them\n"
    "            was always the stale one. */}\n",
    "",
)
text = text.replace(
    "留空表示不选目录，对话模式不会读写任何文件。",
    "留空表示会话没有工作目录，工具仍按项目范围受限。",
)
with io.open(path, "w", encoding="utf-8", newline="\n") as handle:
    handle.write(text)
print("Composer.tsx: the mode prop is gone and work mode is written at the seam")

# --- the two parents stop holding a mode they cannot change --------------------
for name, panel in (("NewSessionPanel.tsx", "new"), ("SessionView.tsx", "session")):
    full = os.path.join(SRC, "components", name)
    with io.open(full, encoding="utf-8") as handle:
        body = handle.read()
    body = body.replace('import { ModeToggle, type Mode } from "./ModeToggle";\n', "")
    body = re.sub(r"  const \[mode, setMode\] = useState<Mode>\(\"chat\"\);\n", "", body)
    body = re.sub(r"\s*<ModeToggle value=\{mode\} onChange=\{setMode\} />\n", "\n", body)
    body = re.sub(r"\n\s*mode=\{mode\}", "", body)
    body = body.replace("              session={{ mode }}\n", "")
    body = body.replace("        // Both of these are drafts. Choosing a mode, or a directory, and then closing the tab\n"
                        "        // should not lose the choice — the person came back to this tab to carry on, not to\n"
                        "        // start again.\n",
                        "        // A draft like any other: choosing a directory and then closing the tab should not\n"
                        "        // lose the choice.\n")
    body = re.sub(
        r" \* The mode sits above the composer container.*?\n \*/\n",
        " */\n",
        body,
        count=1,
        flags=re.S,
    )
    body = re.sub(
        r"        \{/\* The mode row and the composer are one column.*?\*/\}\n",
        "",
        body,
        count=1,
        flags=re.S,
    )
    body = body.replace(
        " * A card in the middle of an otherwise empty tab, because there is nothing to show\n"
        " * until there is a task. Above the card, on its own, is the mode the task will run in —\n"
        " * not on the card, and not in the row of buttons under the input, because the mode is\n"
        " * not part of the form. It is the answer to a question about the message that has not\n"
        " * been written yet, and it changes what sending it will do.\n",
        " * A card in the middle of an otherwise empty tab, because there is nothing to show\n"
        " * until there is a task. The card is the whole interface here rather than one control\n"
        " * among several, so it is centred and given the window's only raised surface.\n",
    )
    with io.open(full, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(body)
    print(f"{name}: no mode state, no mode control")
