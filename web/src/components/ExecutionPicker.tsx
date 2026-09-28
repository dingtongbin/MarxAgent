import { useState } from "react";
import { ChevronDown, Hammer, ListChecks, Unlock } from "lucide-react";
import d from "./NewSessionPanel.module.css";

/**
 * The three levels a coding turn can run at, as the composer offers them.
 *
 * The words a list item carries are the level's name on the first line and one
 * short sentence of what it reaches under it. The sentence is on its own line
 * because a level's reach is not a footnote to its name, and it is short and
 * plain because a list of three is read at a glance or not at all.
 */
const levels = [
  {
    value: "plan",
    name: "计划模式",
    description: "只读代码与检索，不写文件不执行命令",
    icon: ListChecks,
  },
  {
    value: "build",
    name: "构建模式",
    description: "可读写项目文件并在项目内执行命令",
    icon: Hammer,
  },
  {
    value: "full",
    name: "完全访问",
    description: "不设门禁，模型可执行任何操作",
    icon: Unlock,
  },
] as const;

export type ExecutionLevel = (typeof levels)[number]["value"];

/**
 * The execution mode control.
 *
 * It sits beside the send row's attachments control because the level is a
 * property of the turn, like the model beside it: choosing a level is choosing
 * what the words about to be sent are allowed to do. The menu opens upward,
 * from the row it was opened in.
 *
 * The level is chosen before the send, and it travels with the turn: switching
 * levels mid-conversation changes nothing that has already run, and the turn
 * sent next is the one that runs under the level shown here.
 */
export function ExecutionPicker({
  value,
  onChange,
}: {
  value: string;
  onChange: (level: ExecutionLevel) => void;
}) {
  const [open, setOpen] = useState(false);
  const current = levels.find((level) => level.value === value) ?? levels[1];
  const CurrentIcon = current.icon;

  return (
    <div className={d.anchor}>
      <button
        className={`${d.chip} ${d.execChip} ${open ? d.chipOn : ""}`}
        onClick={() => setOpen(!open)}
        aria-haspopup="menu"
        aria-expanded={open}
        title="执行模式"
      >
        <CurrentIcon size={14} strokeWidth={2} />
        <span className={d.chipText}>{current.name}</span>
        <ChevronDown size={13} strokeWidth={2} className={d.chipChev} />
      </button>

      {open ? (
        <>
          <div className={d.shield} onClick={() => setOpen(false)} />
          <div className={`${d.upMenu} ${d.upMenuLeft} ${d.execMenu}`} role="menu">
            {levels.map((level) => {
              const Icon = level.icon;
              return (
                <button
                  key={level.value}
                  role="menuitemradio"
                  aria-checked={level.value === value}
                  className={`${d.menuItem} ${d.execItem} ${
                    level.value === value ? d.menuOn : ""
                  }`}
                  onClick={() => {
                    onChange(level.value);
                    setOpen(false);
                  }}
                  title={level.description}
                >
                  <span className={d.execNameRow}>
                    <Icon size={14} strokeWidth={2} />
                    {level.name}
                  </span>
                  <span className={d.menuSub}>{level.description}</span>
                </button>
              );
            })}
          </div>
        </>
      ) : null}
    </div>
  );
}
