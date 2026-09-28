"""Take the mode control out of the composer, for the last time.

It is in neither of the two places inside the container. It is not on the card's top
edge, because there it is a fourth field in a form. It is not in the row under the
input, because there it is buried among the buttons that act on a message. It is above
the whole container, in its own row, because the mode is a statement about the message
that is about to be written and not a part of it.

So this leaves the composer holding only the input and the controls that act on what is
in it. The mode still travels with the submit, so it becomes a prop rather than
something the composer decides for itself.
"""

import io
import os

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
path = os.path.join(SRC, "components", "Composer.tsx")

with io.open(path, encoding="utf-8") as handle:
    text = handle.read()

# --- the chip in the row under the input --------------------------------------
start = text.find("          <div className={d.anchor}>\n            <button\n              className={`${d.chip} ${modeOpen ? d.chipOn : \"\"}`}")
if start < 0:
    raise SystemExit("mode chip in the button row not found")
# It runs to the end of the left-hand group, which is the line before the right-hand one.
end = text.find("        <div className={d.right}>")
if end < 0 or end < start:
    raise SystemExit("right-hand group not found")
text = text[:start] + text[end:]

# --- the state and the table it needed ----------------------------------------
text = text.replace('import { useEffect, useRef, useState } from "react";\n',
                    'import { useEffect, useRef, useState } from "react";\nimport type { Mode } from "./ModeToggle";\n')
text = text.replace('/** The two modes the project has. They differ in what they are allowed to touch. */\nexport type Mode = "chat" | "work";\n\n', "")
text = text.replace('const modes: { value: Mode; label: string; hint: string }[] = [\n  { value: "chat", label: "对话", hint: "只对话，不读写你的文件" },\n  { value: "work", label: "工作", hint: "在你指定的目录里读写" },\n];\n\n', "")
text = text.replace('  const [mode, setMode] = useState<Mode>(session?.mode ?? "chat");\n  const [modeOpen, setModeOpen] = useState(false);\n', "")
text = text.replace("  const current = modes.find((m) => m.value === mode) ?? modes[0];\n", "")

# --- it is read now, not chosen -----------------------------------------------
text = text.replace("  workdir,\n  onWorkdirChange,\n  onSubmit,",
                    "  workdir,\n  onWorkdirChange,\n  mode,\n  onSubmit,")
text = text.replace("""  workdir: string;
  onWorkdirChange: (path: string) => void;
  onSubmit: (submit: ComposerSubmit) => void;""",
                    """  workdir: string;
  onWorkdirChange: (path: string) => void;
  /** The mode, chosen in the row above this container. */
  mode: Mode;
  onSubmit: (submit: ComposerSubmit) => void;""")

# The left of the row has one control in it now, and saying so stops the next person
# from looking for the one that was there.
text = text.replace("        <div className={d.left}>",
                    "        {/* One control on this side now. The mode was here as well at one\n"
                    "            point, and having the same control in two places meant one of them\n"
                    "            was always the stale one. */}\n"
                    "        <div className={d.left}>")

with io.open(path, "w", encoding="utf-8", newline="\n") as handle:
    handle.write(text)

print("Composer.tsx: the mode control is gone from the container")
for leftover in ("modeOpen", "setMode(", "current.label", "d.modeIcon", "d.modeItem", "modes.map"):
    if leftover in text:
        raise SystemExit(f"leftover reference: {leftover}")
print("Composer.tsx: no leftover references")
