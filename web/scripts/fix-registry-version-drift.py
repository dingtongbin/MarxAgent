"""Make the vendored components compile against the versions actually installed.

Three things are wrong, and all three are consequences of a version moving rather than
of anything being wrong with the components themselves.

The first is usage fields. `ai@7` grouped the token counts: what used to sit on the
usage object directly now lives under the half of the request it belongs to. The
components were written against `ai@6`, where `reasoningTokens` and `cachedInputTokens`
were top level, so the same number now lives at a different path.

The second is a spread of collapsible props into a markdown renderer. `Reasoning`
forwards whatever it was given, and its caller may have passed the open state or a
style meant for the container, neither of which the renderer has a use for.

The third is `@ts-expect-error` directives that suppress nothing. They were written
against a looser configuration; here the code they were silencing is already accepted,
and a directive that suppresses no error is a claim about the code that is no longer
true.
"""

import io
import os
import re

SRC = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")

# --- 1. the usage fields -----------------------------------------------------
CONTEXT = os.path.join(SRC, "components", "ai-elements", "context.tsx")
with io.open(CONTEXT, encoding="utf-8") as handle:
    context = handle.read()

before = context
# Reasoning tokens are produced by the model, so they are counted against output.
context = context.replace(
    "usage?.reasoningTokens ?? 0",
    "usage?.outputTokenDetails?.reasoningTokens ?? 0",
)
# Cache reads are a saving on the way in, so they are counted against input.
context = context.replace(
    "usage?.cachedInputTokens ?? 0",
    "usage?.inputTokenDetails?.cacheReadTokens ?? 0",
)
assert context != before, "no usage field matched in context.tsx"
with io.open(CONTEXT, "w", encoding="utf-8", newline="\n") as handle:
    handle.write(context)
print("context.tsx: usage fields regrouped for ai@7")

# --- 2. the spread into the renderer -----------------------------------------
REASONING = os.path.join(SRC, "components", "ai-elements", "reasoning.tsx")
with io.open(REASONING, encoding="utf-8") as handle:
    reasoning = handle.read()

old = "<Streamdown {...props}>{children}</Streamdown>"
new = (
    "        {/* Only the child's text and class go to the renderer. The rest of the\n"
    "            props belong to the collapsible around it, and forwarding the open state\n"
    "            or a container style into a markdown renderer asks it to honour two things\n"
    "            it has no meaning for. */}\n"
    "        <Streamdown className={className}>{children}</Streamdown>"
)
if old in reasoning:
    reasoning = reasoning.replace(old, new)
    with io.open(REASONING, "w", encoding="utf-8", newline="\n") as handle:
        handle.write(reasoning)
    print("reasoning.tsx: spread narrowed to what the renderer takes")
else:
    print("reasoning.tsx: already narrowed, left alone")

# --- 3. the directives that suppress nothing --------------------------------
removed = 0
for base, _dirs, files in os.walk(SRC):
    for name in files:
        if not name.endswith((".ts", ".tsx")):
            continue
        full = os.path.join(base, name)
        with io.open(full, encoding="utf-8") as handle:
            text = handle.read()
        if "@ts-expect-error" not in text:
            continue
        lines = text.split("\n")
        kept = [line for line in lines if "@ts-expect-error" not in line]
        if len(kept) == len(lines):
            continue
        with io.open(full, "w", encoding="utf-8", newline="\n") as handle:
            handle.write("\n".join(kept))
        removed += len(lines) - len(kept)
        print(f"  {os.path.relpath(full, SRC)}: {len(lines) - len(kept)} removed")
print(f"removed {removed} directives that suppressed nothing")
