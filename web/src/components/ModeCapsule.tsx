import { MessagesSquare, SquareTerminal } from "lucide-react";
import type { LaunchMode } from "./Composer";
import d from "./NewSessionPanel.module.css";

const modes: { value: LaunchMode; label: string; icon: typeof SquareTerminal; title: string }[] = [
  {
    value: "chat",
    label: "对话",
    icon: MessagesSquare,
    title: "对话模式：没有本地文件访问，会话数据存在应用数据目录",
  },
  {
    value: "coding",
    label: "编程",
    icon: SquareTerminal,
    title: "编程模式：会话挂在一个项目目录上，可以读写文件、检索代码并执行构建命令",
  },
];

/**
 * The capsule that switches the launcher's mode.
 *
 * It sits above the composer because the mode is what the composer is made of, not
 * one of its parts: a chooser among the parts would read as a setting of the current
 * form, and changing it would then reasonably be expected to keep the form. It does
 * not — each mode is its own container, mounted on its own key, and nothing typed in
 * one is waiting in the other.
 *
 * The two modes are the two this build ships. What is to come is not drawn as a
 * disabled third capsule: a control that is there but does nothing says the window is
 * broken, while an absence says only that the thing is not here yet.
 */
export function ModeCapsule({
  value,
  onChange,
}: {
  value: LaunchMode;
  onChange: (mode: LaunchMode) => void;
}) {
  return (
    <div className={d.modeCapsule} role="radiogroup" aria-label="会话模式">
      {modes.map((entry) => {
        const on = entry.value === value;
        const Icon = entry.icon;
        return (
          <button
            key={entry.value}
            role="radio"
            aria-checked={on}
            className={`${d.modeCapsuleItem} ${on ? d.modeCapsuleOn : ""}`}
            onClick={() => onChange(entry.value)}
            title={entry.title}
          >
            <Icon size={14} strokeWidth={2} />
            <span>{entry.label}</span>
          </button>
        );
      })}
    </div>
  );
}
