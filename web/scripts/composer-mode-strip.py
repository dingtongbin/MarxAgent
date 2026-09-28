"""Take the mode control out of the composer and put it above the container.

The mode is not a field in the form. It is the answer to a question about the message
before there is a message, and it decides what sending will do. It was in two wrong
places — on the card's top edge, which made it a fourth field, and in the row under the
input, which buried it among the buttons that act on a message — and is now its own
element above the whole container.

So this removes it from the composer's top edge and from the composer's bottom row, and
leaves the composer holding only the input and the controls that act on what is in it.
The mode still travels with the submit, because the server needs to know it, so the
composer keeps taking it as a prop rather than owning it.
"""

import io
import os

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")
path = os.path.join(SRC, "components", "Composer.tsx")

with io.open(path, encoding="utf-8") as handle:
    text = handle.read()

# --- the block that is now wrong, located by its own distinctive opening ---------
start_marker = "      {/*\n       * The top edge, above the input rather than inside it."
end_marker = "        <div className={d.anchor}>\n          <button\n            className={`${d.chip} ${dirOpen ? d.chipOn : \"\"}"

start = text.find(start_marker)
if start < 0:
    raise SystemExit("top-edge block not found; the file has changed shape")
end = text.find(end_marker)
if end < 0:
    raise SystemExit("directory anchor not found; the file has changed shape")

replacement = (
    "      {/* The top edge of the container, and it carries one thing: the directory.\n"
    "          The mode is not here. It sits in its own row above the whole container,\n"
    "          because it is a statement about the message rather than a field of the\n"
    "          form, and a statement that changes what sending does does not belong\n"
    "          inside the thing being sent. */}\n"
    "      <div className={d.topRow}>\n"
)
text = text[:start] + replacement + text[end:]

# --- the mode chip in the row under the input ----------------------------------
mode_start = text.find("          <div className={d.anchor}>\n            <button\n              className={`${d.chip} ${modeOpen ? d.chipOn : \"\"}`}")
if mode_start < 0:
    raise SystemExit("mode chip in the button row not found")
# The block ends at the closing of the right-hand group, which is the line before the
# one that opens it.
group_close = text.find("        </div>\n\n        <div className={d.right}>")
if group_close < 0 or group_close < mode_start:
    raise SystemExit("right-hand group boundary not found")
text = text[:mode_start] + text[group_close:]

# --- the state and the modes table it needed ------------------------------------
text = text.replace(
    'import { useEffect, useRef, useState } from "react";\n',
    'import { useEffect, useRef, useState } from "react";\nimport type { Mode } from "./ModeToggle";\n',
)
text = text.replace(
    "/** The two modes the project has. They differ in what they are allowed to touch. */\n"
    "export type Mode = \"chat\" | \"work\";\n\n",
    "",
)
text = text.replace(
    "const modes: { value: Mode; label: string; hint: string }[] = [\n"
    '  { value: "chat", label: "对话", hint: "只对话，不读写你的文件" },\n'
    '  { value: "work", label: "工作", hint: "在你指定的目录里读写" },\n'
    "];\n\n",
    "",
)
# `current` was the label for the chip that is no longer in here.
text = text.replace("  const current = modes.find((m) => m.value === mode) ?? modes[0];\n", "")
# The mode is read, not chosen, here.
text = text.replace("  const [mode, setMode] = useState<Mode>(session?.mode ?? \"chat\");\n", "")
text = text.replace(
    "  onSubmit,\n  running,\n  onInterrupt,\n  session,\n}: {",
    "  mode,\n  onSubmit,\n  running,\n  onInterrupt,\n  session,\n}: {",
)
text = text.replace(
    "  /** What a running session already decided, shown when the variant is \"session\". */\n  session?:",
    "  /** The mode, chosen in the row above this container. */\n  mode: Mode;\n  /** What a running session already decided, shown when the variant is \"session\". */\n  session?:",
)
# What is left in the button row on the left is the plus button alone.
text = text.replace(
    "        <div className={d.left}>",
    "        {/* The left of this row holds one control now. The mode used to be here as\n"
    "            well, and having the same control in two rows meant one of them was\n"
    "            always the stale one. */}\n"
    "        <div className={d.left}>",
)

with io.open(path, "w", encoding="utf-8", newline="\n") as handle:
    handle.write(text)

print("Composer.tsx: mode control removed from the container")
for leftover in ("d.modeIcon", "d.modeItem", "modes.map", "setMode(", "current.label"):
    if leftover in text:
        raise SystemExit(f"leftover reference to the removed control: {leftover}")
print("Composer.tsx: no leftover references")
