import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { ChevronRight, Folder, FolderOpen, Loader2, X } from "lucide-react";
import { browseDir, type DirEntry } from "@/api";
import { ScrollArea } from "@/components/ui/scroll-area";
import s from "./DirPicker.module.css";

/**
 * A folder picker, drawn as the tree the filesystem already is.
 *
 * It opens at the top of the machine — the drives on Windows, the root everywhere else —
 * and **nothing else is read**. Every folder below that is asked for at the moment it
 * is opened, and once: a disk holds more folders than are worth reading before
 * anybody has asked for them, and reading them all to draw two rows is work whose cost
 * is paid before the dialog has shown anything.
 *
 * The tree is written here rather than taken from a library, and the reason is the
 * contract of the library that was tried: its visible rows are rebuilt only when its own
 * open state changes, so a tree whose data is 懒读 from a server — where expansion is
 * decided by this window, not by the tree — never recomputes what to draw and answers
 * every click with the rows it started with. A tree that renders what its data says is a
 * flat list and a loop: the rows in order, the depth of each, and everything else
 * (expansion, reading, the keyboard) is state this file can read and test.
 */

/** One node. The path is also the identity: a name is not unique, a path is. */
type Item = {
  id: string;
  name: string;
  path: string;
  /**
   * Always present, even before the folder has been read.
   *
   * Every folder is created as an internal node with nothing in it, and `loaded` is what
   * tells an empty folder apart from one nobody has read yet: a folder that has been read
   * and holds nothing is a closed door, and one not read at all is a door nobody has
   * tried.
   */
  children: Item[];
  loaded?: boolean;
  /** Set while this folder is being read, so the row it opened onto can say so. */
  reading?: boolean;
};

/** One row on screen: a folder and how far it is indented. */
type Row = { item: Item; depth: number };

/** The name the machine's top is given at the foot before anything is chosen. */
const machineRoot = "此电脑";

/** The height of one row, in pixels. The scroll of the tree is counted in these. */
const rowHeight = 28;

/**
 * How long one folder may take to be read before the row is failed.
 *
 * Eight seconds is generous for listing a directory on a local machine — a spinning
 * disk over a network share is the slow case, and that is the case worth waiting for.
 * It is short enough that a request which has gone somewhere and will not come back
 * stops looking like work in progress.
 */
const readBudget = 8_000;

/**
 * A listing becomes the children of one folder.
 *
 * The path is the server's, whole. Nothing here joins a name onto a parent: a path is
 * the machine's to know, and the two halves of a Windows path do not look like the two
 * halves of a POSIX one.
 */
function toItems(entries: DirEntry[]): Item[] {
  return entries.map((entry) => ({
    id: entry.path,
    name: entry.name,
    path: entry.path,
    children: [],
  }));
}

/**
 * Put a folder's children under the node that owns it.
 *
 * The walk is by path and not by name, because a name repeats and a path does not.
 * Every node along the way is rebuilt rather than changed in place: the rows on screen
 * were drawn from the previous tree, and changing that one in place would leave the two
 * disagreeing with nothing to repaint from.
 *
 * `reading` is set to false rather than left to come along. Spreading the old node
 * carries the flag onto the new one, and a row that was read is then still turning for
 * ever — a spinner whose only cause is the code that was supposed to stop it.
 */
function graft(items: Item[], path: string, children: Item[]): Item[] {
  return items.map((item) => {
    if (item.path === path) return { ...item, children, loaded: true, reading: false };
    if (item.children.length === 0) return item;
    return { ...item, children: graft(item.children, path, children) };
  });
}

/**
 * Set a flag on one folder, leaving everything else as it was.
 *
 * The flag lives in the data rather than in the row's own state because the rows are
 * drawn from this tree: a spinner held in a ref or in a state beside the tree would be
 * correct and invisible, because nothing would tell the rows to draw again until the
 * read they were waiting for had already finished.
 */
function flagged(items: Item[], path: string, patch: Partial<Item>): Item[] {
  return items.map((item) => {
    if (item.path === path) return { ...item, ...patch };
    if (item.children.length === 0) return item;
    return { ...item, children: flagged(item.children, path, patch) };
  });
}

/**
 * The rows on screen, in the order they are drawn.
 *
 * A folder that is not open contributes itself and stops: what is under it is never
 * drawn. A folder that is open contributes its children after it, each at one more
 * indent. This is the whole of what a tree is — a depth-first walk that stops at closed
 * doors — and writing it here means a row that appears is a row this function put on
 * the list, which is the kind of thing that can be asserted in a test.
 */
function flatten(items: Item[], open: ReadonlySet<string>, depth = 0): Row[] {
  const rows: Row[] = [];
  for (const item of items) {
    rows.push({ item, depth });
    if (open.has(item.path) && item.children.length > 0) {
      rows.push(...flatten(item.children, open, depth + 1));
    }
  }
  return rows;
}

/** The folder at a path, or null. `id` is the path, so one walk answers both. */
function at(items: Item[], path: string): Item | null {
  for (const item of items) {
    if (item.path === path) return item;
    if (item.children.length > 0) {
      const inside = at(item.children, path);
      if (inside) return inside;
    }
  }
  return null;
}

export function DirPicker({
  open,
  onCancel,
  onPick,
}: {
  open: boolean;
  onCancel: () => void;
  onPick: (path: string) => void;
}) {
  const [items, setItems] = useState<Item[]>([]);
  // Which folders are open. Nothing else decides what is drawn: the rows below come
  // straight out of this set and the tree it is read from.
  const [openPaths, setOpenPaths] = useState<ReadonlySet<string>>(() => new Set());
  // The row the keyboard is on, by path. Clicking moves it too, so the two ways in
  // agree on where the ring is.
  const [focused, setFocused] = useState("");
  const [picked, setPicked] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [box, setBox] = useState<{ width: number; height: number }>({ width: 560, height: 320 });
  const boxRef = useRef<HTMLDivElement>(null);
  // The folders being read right now, so opening and closing one quickly does not ask
  // for it twice, and so a folder that is already loaded is never asked for at all.
  const readingNow = useRef(new Set<string>());
  // The tree holds its own copy of the data, and the grafting needs the current one to
  // build the next from. A ref is the only way to get at it from a callback that must
  // not be re-created.
  const itemsRef = useRef<Item[]>([]);

  /** Read one folder, once, and put what came back under it. */
  const read = useCallback((path: string) => {
    const folder = at(itemsRef.current, path);
    if (!folder || folder.loaded) return;
    if (readingNow.current.has(path)) return;
    readingNow.current.add(path);
    // The folder is marked as being read before the request goes out, so a slow drive
    // shows a row that is working rather than a row that opened onto nothing.
    itemsRef.current = flagged(itemsRef.current, path, { reading: true });
    setItems(itemsRef.current);
    // A read that never comes back must not leave a row turning for ever. The whole
    // request already has a budget of its own, but that is a minute of nothing, and a
    // minute of a spinner is a window that has stopped answering a question. This is
    // the shorter one: past it, the row is failed and says so rather than pretending.
    const expiry = setTimeout(() => {
      if (!readingNow.current.has(path)) return;
      readingNow.current.delete(path);
      setErr(`${path} 读取超时，目录可能打不开或所在设备没有响应。`);
      itemsRef.current = flagged(itemsRef.current, path, { reading: false });
      setItems(itemsRef.current);
    }, readBudget);
    browseDir(path)
      .then((listing) => {
        clearTimeout(expiry);
        const next = graft(itemsRef.current, path, toItems(listing.entries));
        itemsRef.current = next;
        setItems(next);
      })
      .catch((cause: unknown) => {
        clearTimeout(expiry);
        // The server says what went wrong in words, and those words are the only thing
        // that can tell a person whether to fix the path or their permissions. They are
        // also written to the console, because a dialog that fails silently is a dialog
        // whose failure has to be guessed at from the outside.
        const message = cause instanceof Error ? cause.message : String(cause);
        console.error(`browse ${path} failed`, cause);
        setErr(message);
        itemsRef.current = flagged(itemsRef.current, path, { reading: false });
        setItems(itemsRef.current);
      })
      .finally(() => {
        readingNow.current.delete(path);
      });
  }, []);

  /**
   * Open or close a folder, reading it the first time it is opened.
   *
   * Reading on the way in rather than on the way out is what makes a folder nobody has
   * looked inside show its contents: the moment a person opens it is the moment they
   * need to have asked for them. An empty folder that has been read stays empty and is
   * not asked for again.
   */
  const toggle = useCallback(
    (path: string) => {
      setPicked(path);
      setFocused(path);
      setOpenPaths((current) => {
        const next = new Set(current);
        if (next.has(path)) {
          next.delete(path);
          return next;
        }
        next.add(path);
        read(path);
        return next;
      });
    },
    [read],
  );

  const rows = useMemo(() => flatten(items, openPaths), [items, openPaths]);

  // Opened at the top of the machine, and nothing below it. The empty path is the
  // server's way of asking "where do I start", and it is the only path that is not
  // scoped to anything.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setErr(null);
    setPicked("");
    setLoading(true);
    setOpenPaths(new Set());
    setFocused("");
    readingNow.current.clear();
    itemsRef.current = [];

    browseDir("")
      .then((listing) => {
        if (cancelled) return;
        // The drives are the only thing read here, and they are read as folders with
        // nothing in them rather than as folders whose contents were fetched and thrown
        // away: what is inside D: is asked for when somebody opens D:.
        const roots = toItems(listing.entries).map((item) => ({ ...item, loaded: false }));
        itemsRef.current = roots;
        setItems(roots);
        setLoading(false);
      })
      .catch((cause: unknown) => {
        if (cancelled) return;
        console.error("browse the top of the machine failed", cause);
        setErr(cause instanceof Error ? cause.message : String(cause));
        setLoading(false);
      });

    // Opening the dialog again, or leaving it while a folder is being read, both mean
    // the answer is no longer wanted. Without this a slow read lands on a dialog that
    // has been closed and a tree that has been thrown away.
    return () => {
      cancelled = true;
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") onCancel();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onCancel]);

  // The tree is told how much room it has rather than being left to guess, and the
  // number is measured because the dialog's height is set in the stylesheet and is not
  // knowable from here.
  useEffect(() => {
    if (!open) return;
    const measure = () => {
      const element = boxRef.current;
      if (element) setBox({ width: element.clientWidth, height: element.clientHeight });
    };
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [open]);

  return (
    <Dialog.Root open={open} onOpenChange={(next) => !next && onCancel()}>
      <Dialog.Portal>
        {/* The overlay darkens everything behind, and the content is one layer above it
            so the dialog is not darkened by its own backdrop. */}
        <Dialog.Overlay className={s.overlay} />
        <Dialog.Content className={s.content} aria-describedby={undefined}>
          <div className={s.head}>
            <Dialog.Title className={s.title}>选择文件夹</Dialog.Title>
            <Dialog.Close asChild>
              <button className={s.close} aria-label="关闭">
                <X size={16} strokeWidth={2} />
              </button>
            </Dialog.Close>
          </div>

          {err ? <div className={s.err}>{err}</div> : null}

          {/* A fixed height with the tree filling it, so a folder with thousands of
              entries scrolls inside the dialog rather than pushing the buttons off the
              bottom of the screen. */}
          <div className={s.body} ref={boxRef}>
            {loading ? (
              <div className={s.spinning}>
                <Loader2 className={s.spin} size={15} strokeWidth={2} />
              </div>
            ) : null}
            {!loading && items.length === 0 && !err ? (
              <div className={s.spinning}>
                <Folder size={15} strokeWidth={2} />
                <span>没有可打开的位置</span>
              </div>
            ) : null}
            {items.length > 0 ? (
              <Rows
                rows={rows}
                openPaths={openPaths}
                focused={focused}
                width={box.width}
                height={box.height}
                onToggle={toggle}
                onMove={setFocused}
              />
            ) : null}
          </div>

          <div className={s.foot}>
            <span className={s.picked} title={picked}>
              {picked || machineRoot}
            </span>
            <button className={s.ghost} onClick={onCancel}>
              取消
            </button>
            <button className={s.primary} disabled={!picked} onClick={() => onPick(picked)}>
              <FolderOpen size={15} strokeWidth={2} />
              选择当前文件夹
            </button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/**
 * The rows on screen, and only those.
 *
 * A listing of `C:\Windows\System32` is four thousand rows and a dialog can show twenty
 * of them, so the container is `height: rows × rowHeight` — the scroll the rows would
 * take up if all were drawn — and the rows themselves are windowed: only those inside
 * the scroll are on the page, each absolutely positioned where it belongs. Scrolling
 * reads `scrollTop` and changes which rows exist, which is the whole of it.
 */
function Rows({
  rows,
  openPaths,
  focused,
  width,
  height,
  onToggle,
  onMove,
}: {
  rows: Row[];
  openPaths: ReadonlySet<string>;
  focused: string;
  width: number;
  height: number;
  onToggle: (path: string) => void;
  onMove: (path: string) => void;
}) {
  const [scroll, setScroll] = useState(0);
  // The element that scrolls is the scrollbar component's viewport, and its scroll
  // events do not bubble — React's onScroll on the wrapper would never hear them. The
  // listener goes on the viewport itself, found once it is on the page.
  const holdRef = useRef<HTMLDivElement>(null);
  const viewportRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    const holder = holdRef.current;
    if (!holder) return;
    const viewport = holder.querySelector<HTMLElement>('[data-slot="scroll-area-viewport"]');
    if (!viewport) return;
    viewportRef.current = viewport;
    const onScroll = () => setScroll(viewport.scrollTop);
    viewport.addEventListener("scroll", onScroll, { passive: true });
    return () => {
      viewport.removeEventListener("scroll", onScroll);
      viewportRef.current = null;
    };
  }, []);
  const overscan = 6;
  const first = Math.max(0, Math.floor(scroll / rowHeight) - overscan);
  const last = Math.min(rows.length, Math.ceil((scroll + height) / rowHeight) + overscan);
  const shown = rows.slice(first, last);

  return (
    // The holder is this file's own element: measured by the dialog, and the place the
    // keyboard lands. The scrollbar component fills it and does the scrolling.
    <div
      tabIndex={0}
      role="tree"
      aria-label="文件夹"
      className={s.tree}
      style={{ height, width }}
      ref={holdRef}
      onKeyDown={(event) => {
        const found = (event.target as HTMLElement | null)?.dataset?.path;
        if (!found) {
          // The keys belong to the rows. A key arriving from the holder itself, with
          // no row under it, is not a word the tree answers.
          return;
        }
        const path = found;
        const at = rows.findIndex((row) => row.item.path === path);
        if (at < 0) return;
        const move = (to: number) => {
          event.preventDefault();
          onMove(rows[to].item.path);
          // The row the keyboard moved to is scrolled to, because focus that is out of
          // sight is a selection the person cannot see.
          const viewport = viewportRef.current;
          if (!viewport) return;
          const top = to * rowHeight;
          if (top < viewport.scrollTop) viewport.scrollTop = top;
          else if (top + rowHeight > viewport.scrollTop + height) {
            viewport.scrollTop = top + rowHeight - height;
          }
        };
        if (event.key === "ArrowDown" && at < rows.length - 1) move(at + 1);
        else if (event.key === "ArrowUp" && at > 0) move(at - 1);
        else if (event.key === "ArrowRight" && !openPaths.has(path)) {
          event.preventDefault();
          onToggle(path);
        } else if (event.key === "ArrowLeft" && openPaths.has(path)) {
          event.preventDefault();
          onToggle(path);
        } else if (event.key === "Enter") {
          event.preventDefault();
          onMove(path);
          onToggle(path);
        }
      }}
    >
      <ScrollArea className={s.treeFill}>
        {/* The scroll is the rows' height, whether they exist or not, so the bar the
            wrapper draws is the size of the tree and not of the window. */}
        <div style={{ height: rows.length * rowHeight, position: "relative" }}>
          {shown.map((row, index) => (
            <RowLine
              key={row.item.path}
              row={row}
              top={(first + index) * rowHeight}
              width={width}
              open={openPaths.has(row.item.path)}
              focused={focused === row.item.path}
              onToggle={onToggle}
              onMove={onMove}
            />
          ))}
        </div>
      </ScrollArea>
    </div>
  );
}

/**
 * One row, absolutely positioned where the list says it is.
 *
 * Clicking a folder opens it, because that is what clicking a folder in a tree is: the
 * path fills in at the foot, the folder's contents are read, and the rows under it
 * appear — three things, all of them this file's own doing.
 */
function RowLine({
  row,
  top,
  width,
  open,
  focused,
  onToggle,
  onMove,
}: {
  row: Row;
  top: number;
  width: number;
  open: boolean;
  focused: boolean;
  onToggle: (path: string) => void;
  onMove: (path: string) => void;
}) {
  const { item, depth } = row;
  return (
    // The row is a button's worth of markup rather than a list item because the whole
    // row is one thing to push: a folder row that opened only when the glyph was hit
    // would be a dialog that answers some clicks and swallows the rest.
    <div
      role="treeitem"
      aria-expanded={item.children.length > 0 ? open : undefined}
      aria-selected={focused}
      aria-level={depth + 1}
      data-path={item.path}
      tabIndex={focused ? 0 : -1}
      className={focused ? `${s.row} ${s.rowFocus}` : s.row}
      style={{
        position: "absolute",
        top,
        height: rowHeight,
        width,
        paddingLeft: 12 + depth * 18,
      }}
      onClick={() => {
        // The click is both a choice and an opening: the name goes to the foot, and
        // what is under the folder comes into the tree. Selecting and opening are what
        // one click in a tree does, not two.
        onMove(item.path);
        if (item.children.length > 0 || !item.loaded) onToggle(item.path);
      }}
      onFocus={(event) => event.stopPropagation()}
    >
      <span className={open ? s.chevOpen : s.chev} aria-hidden="true">
        {/* The arrow points down when the folder is open and right otherwise, which is
            the one picture of "open" that needs no words. */}
        <ChevronRight size={14} strokeWidth={2} />
      </span>
      {item.reading ? (
        <Loader2 size={14} strokeWidth={2} className={s.spin} />
      ) : (
        <Folder size={14} strokeWidth={1.75} className={s.dir} />
      )}
      <span className={s.name}>{item.name}</span>
    </div>
  );
}
