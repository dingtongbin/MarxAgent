"""Rewrite the monorepo's own import paths to this project's aliases.

The registry copies the source files verbatim, and inside the upstream repository those
files import each other through the workspace package name rather than through a path.
The registry rewrites the ones it knows about and leaves the rest, so a vendored file
still says `@repo/shadcn-ui/lib/utils` and TypeScript cannot resolve it.

Rewriting them here is a search-and-replace rather than a per-file edit because the
alternative is opening sixty files to change one line in each, and a change made by a
script is the same change a person would make, with the advantage of not being missed
in the ones nobody opened.
"""

import io
import os
import re
import sys

# The script lives in web/scripts, so the sources are one level up rather than two.
ROOT = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src")

# Longest first: the directory form has to be tried before the bare package name, or
# `@repo/shadcn-ui/lib/utils` would match the package prefix and leave `/lib/utils`
# dangling on the end of an already-substituted path.
RULES = [
    (r"@repo/shadcn-ui/lib/utils", "@/lib/utils"),
    (r"@repo/shadcn-ui/components/ui/", "@/components/ui/"),
    (r"@repo/shadcn-ui/components/", "@/components/"),
    (r"@repo/shadcn-ui/hooks/", "@/hooks/"),
    (r"@repo/shadcn-ui/lib/", "@/lib/"),
    (r"@repo/elements", "@/components/ai-elements"),
]

pattern = re.compile("|".join(rule[0] for rule in RULES))

changed = []
for base, _dirs, files in os.walk(ROOT):
    for name in files:
        if not name.endswith((".ts", ".tsx")):
            continue
        full = os.path.join(base, name)
        with io.open(full, encoding="utf-8") as handle:
            before = handle.read()
        if "@repo/" not in before:
            continue

        def replace(match: "re.Match[str]") -> str:
            for needle, replacement in RULES:
                if match.group(0) == needle or match.group(0).startswith(needle):
                    return replacement + match.group(0)[len(needle):]
            return match.group(0)

        after = pattern.sub(replace, before)
        if after != before:
            with io.open(full, "w", encoding="utf-8", newline="\n") as handle:
                handle.write(after)
            changed.append(os.path.relpath(full, ROOT))

print(f"rewrote {len(changed)} files")
for name in changed[:8]:
    print("  " + name)
if len(changed) > 8:
    print(f"  ... and {len(changed) - 8} more")

# Anything still pointing at the monorepo is a rule that was missed, and it is worth
# saying so rather than leaving it to fail as an unresolved module later.
leftover = []
for base, _dirs, files in os.walk(ROOT):
    for name in files:
        if not name.endswith((".ts", ".tsx")):
            continue
        full = os.path.join(base, name)
        with io.open(full, encoding="utf-8") as handle:
            if "@repo/" in handle.read():
                leftover.append(os.path.relpath(full, ROOT))

if leftover:
    print("still referencing the monorepo:")
    for name in leftover:
        print("  " + name)
    sys.exit(1)
