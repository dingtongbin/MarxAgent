import { useEffect, useState } from "react";
import {
  Check,
  ChevronDown,
  Eye,
  EyeOff,
  MoreHorizontal,
  Pencil,
  Plug,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import {
  ApiError,
  getConfig,
  getProviderCatalog,
  listProviderModels,
  putConfig,
  testProvider,
  type ConfigView,
  type FormatOption,
  type ModelInput,
  type ModelView,
  type Preset,
  type ProviderSpec,
  type ProviderInput,
  type ProviderView,
} from "@/api";
import { useUI, type Theme } from "@/store/ui";
import { useWork } from "@/store/work";
import s from "./SettingsPanel.module.css";

/** The sections, grouped as they are read: what this is, what it talks to, what it
 *  has done. The grouping is the navigation; the order inside it is reading order. */
type SectionId = "general" | "appearance" | "model" | "hotkeys" | "mcp" | "stats";

const NAV: { group: string; items: { id: SectionId; label: string }[] }[] = [
  {
    group: "通用",
    items: [
      { id: "general", label: "通用设置" },
      { id: "appearance", label: "外观设置" },
      { id: "model", label: "模型设置" },
      { id: "hotkeys", label: "键盘快捷键" },
    ],
  },
  {
    group: "Agent 与扩展",
    items: [{ id: "mcp", label: "MCP 服务器管理" }],
  },
  {
    group: "其他",
    items: [{ id: "stats", label: "使用统计" }],
  },
];

const titles: Record<SectionId, string> = Object.fromEntries(
  NAV.flatMap((group) => group.items.map((item) => [item.id, item.label])),
) as Record<SectionId, string>;

const themeOptions: { value: Theme; label: string }[] = [
  { value: "system", label: "跟随系统" },
  { value: "dark", label: "深色" },
  { value: "light", label: "浅色" },
];

/**
 * The settings tab.
 *
 * A title, the sections down the left, the chosen one on the right. Both columns are
 * full height and scroll on their own, so a long section does not take the list of
 * sections with it — which is the moment somebody wants to go somewhere else.
 */
export function SettingsPanel() {
  const theme = useUI((u) => u.theme);
  const setTheme = useUI((u) => u.setTheme);
  // Which section is showing comes from the store rather than from this panel's own
  // memory, because the model picker asks for the model section from outside here.
  // A control elsewhere reaching into a panel that may not even be mounted is not
  // something a control should have to do.
  const section = useWork((state) => state.settingsSection);
  const setSection = useWork((state) => state.setSettingsSection);

  // A section asked for from outside that is not one of them falls back to the
  // first, rather than rendering a page with no heading over it.
  const shown: SectionId = (NAV.flatMap((group) => group.items).some((item) => item.id === section)
    ? section
    : "general") as SectionId;

  return (
    <div className={s.wrap}>
      <div className={s.page}>
        <div className={s.body}>
          <nav className={s.nav} aria-label="设置分区">
            {NAV.map((group) => (
              <div key={group.group} className={s.navGroup}>
                <div className={s.navGroupTitle}>{group.group}</div>
                {group.items.map((item) => (
                  <button
                    key={item.id}
                    className={`${s.navItem} ${shown === item.id ? s.navOn : ""}`}
                    onClick={() => setSection(item.id)}
                    aria-current={shown === item.id ? "page" : undefined}
                  >
                    {item.label}
                  </button>
                ))}
              </div>
            ))}
          </nav>

          <div className={s.content}>
            <h2 className={s.contentTitle}>{titles[shown]}</h2>
            {shown === "general" ? <General /> : null}
            {shown === "appearance" ? <Appearance theme={theme} setTheme={setTheme} /> : null}
            {shown === "model" ? <Model /> : null}
            {shown === "hotkeys" ? <Hotkeys /> : null}
            {shown === "mcp" ? <Mcp /> : null}
            {shown === "stats" ? <Stats /> : null}
          </div>
        </div>
      </div>
    </div>
  );
}

function General() {
  return (
    <>
      {/* Every one of these is a fact about this build rather than a setting, and a
          person who has found a bug needs them before they have found a preference. */}
      <KV label="服务地址" value={window.location.origin} />
      <KV label="版本" value="MarxAgent v0.1.0-dev" />
      <KV label="事件流" value="Server-Sent Events" />
      <KV label="存储" value="JSONL + SQLite" />
      <KV label="许可" value="Apache-2.0" />

    </>
  );
}

function Appearance({
  theme,
  setTheme,
}: {
  theme: Theme;
  setTheme: (theme: Theme) => void;
}) {
  return (
    <>
      <div className={s.segRow}>
        <span className={s.label}>主题</span>
        {/* Three answers to one question, so they are one control to a screen reader
            rather than three buttons it has to weigh up separately. */}
        <div className={s.seg} role="radiogroup" aria-label="主题">
          {themeOptions.map((option) => (
            <button
              key={option.value}
              role="radio"
              aria-checked={theme === option.value}
              className={`${s.segBtn} ${theme === option.value ? s.segOn : ""}`}
              onClick={() => setTheme(option.value)}
            >
              {option.label}
            </button>
          ))}
        </div>
      </div>
      <div className={s.segRow}>
        <span className={s.label}>任务栏宽度</span>
        <span className={s.value}>{useUI.getState().sidebarWidth} px</span>
      </div>
    </>
  );
}

/** What a token count is shown as: 131072 is 128K to a person reading a list. */
function fmtTokens(n: number): string {
  if (!n) return "—";
  return n >= 1024 ? `${Math.round(n / 1024)}K` : String(n);
}

/** The reasoning strengths, as they are stored and as they are read. */
const THINKING_LEVELS = ["off", "low", "medium", "high"] as const;
const THINKING_LABELS: Record<string, string> = {
  off: "关闭",
  low: "低",
  medium: "中",
  high: "高",
};

/** A switch, because a checkbox drawn as a checkbox cannot be read at a glance. */
function Switch({
  on,
  onClick,
  title,
}: {
  on: boolean;
  onClick: () => void;
  title?: string;
}) {
  return (
    <button
      className={`${s.sw} ${on ? s.swOn : ""}`}
      onClick={onClick}
      title={title}
      role="switch"
      aria-checked={on}
    >
      <span className={s.swKnob} />
    </button>
  );
}

/**
 * The model section.
 *
 * A list of providers down the left, the chosen one on the right, and its models
 * below that. Everything is edited in place and written by one button at the foot,
 * because the configuration is one document: a page that saved each field the
 * moment it changed would be a page where half a provider exists.
 *
 * What this page cannot do is read a key back. It is told whether one is held, and
 * it can set or clear one, which is everything except copying a secret out of a
 * file into a browser — which is not something a settings page should be able to
 * do anyway.
 */
function Model() {
  const [view, setView] = useState<ConfigView | null>(null);
  const [draft, setDraft] = useState<Record<string, ProviderInput>>({});
  const [selected, setSelected] = useState<string | null>(null);
  const [catalog, setCatalog] = useState<Preset[] | null>(null);
  const [presetOpen, setPresetOpen] = useState(false);
  const [state, setState] = useState<"loading" | "ready" | "saving" | "saved">("loading");
  const [error, setError] = useState("");
  const [reloading, setReloading] = useState(false);

  // The load runs once for the life of the section. A load on every render would be
  // a page that overwrites what somebody is typing with what is on disk, which is
  // the one thing a settings page must not do.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const loaded = await getConfig();
        if (cancelled) return;
        setView(loaded);
        setDraft(toInputs(loaded.model.providers));
        setSelected(Object.keys(loaded.model.providers).sort()[0] ?? null);
        setState("ready");
      } catch (failure) {
        if (cancelled) return;
        setError(describe(failure));
        setState("ready");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const patch = (name: string, change: Partial<ProviderInput>) =>
    setDraft((current) => ({ ...current, [name]: { ...current[name], ...change } }));

  const editModel = (name: string, model: ModelInput, change: Partial<ModelInput>) =>
    patch(name, {
      models: (draft[name]?.models ?? []).map((existing) =>
        existing.id === model.id ? { ...existing, ...change } : existing,
      ),
    });

  const addModel = (name: string, model: ModelInput) =>
    patch(name, { models: byId([...(draft[name]?.models ?? []), model]) });

  const removeModel = (name: string, id: string) =>
    patch(name, { models: (draft[name]?.models ?? []).filter((model) => model.id !== id) });

  const add = () => {
    // A name that does not collide, so adding twice never silently edits the first
    // one. A person who wanted a second provider of the same kind names it.
    let name = "provider";
    for (let index = 2; draft[name]; index += 1) name = `provider-${index}`;
    setDraft((current) => ({
      ...current,
      [name]: { endpoint: "", format: view?.formats[0]?.value ?? "openai-completions", models: [] },
    }));
    setSelected(name);
    setState("ready");
  };

  // A preset fills in an endpoint, a format, a key variable and a model list. It
  // fills in no key, because a preset that carried one would be a credential in
  // the program.
  const openPresets = () => {
    setPresetOpen((open) => !open);
    if (catalog === null) {
      getProviderCatalog()
        .then(setCatalog)
        .catch(() => setCatalog([]));
    }
  };

  // A preset fills in an endpoint, a format and a key variable. It fills in no key
  // and no models: a key would be a credential in the program, and a model list
  // typed once by somebody would be a second source of truth about somebody else's
  // service. The models come from the endpoint when they are asked for.
  const pickPreset = (preset: Preset) => {
    let name = preset.slug;
    for (let index = 2; draft[name]; index += 1) name = `${preset.slug}-${index}`;
    setDraft((current) => ({
      ...current,
      [name]: {
        name: preset.name,
        enabled: true,
        endpoint: preset.endpoint,
        format: preset.format,
        api_key_env: preset.api_key_env ?? "",
        models: [],
      },
    }));
    setSelected(name);
    setPresetOpen(false);
    setState("ready");
  };

  // The save, and the one thing that makes the rest of this page matter: without it
  // every edit above is a draft that a reload throws away.
  // Writing the configuration. It takes the providers to write rather than reading
  // the draft, because the two callers below write different things: the button
  // writes what is on screen, and a deletion writes what is on screen minus the one
  // that is being removed.
  //
  // The default model is carried over from what is stored rather than sent empty.
  // Sending empty would clear it on every save, and a default that quietly
  // disappears is a default somebody will spend an afternoon wondering about.
  const persist = async (providers: Record<string, ProviderInput>) => {
    setState("saving");
    setError("");
    try {
      const saved = await putConfig({
        model: { default: view?.model.default ?? "", providers },
      });
      setView(saved);
      setDraft(toInputs(saved.model.providers));
      setState("saved");
      return true;
    } catch (failure) {
      setError(describe(failure));
      setState("ready");
      return false;
    }
  };

  const save = () => persist(draft);

  // Removing a provider is written out at once.
  //
  // It is the one edit on this page that destroys something, and making it wait
  // for a second click somewhere else is how a deletion comes back: the row goes,
  // the page says it is gone, and a reload brings it back because nothing was
  // written. The draft is moved first, so the row stays gone even if the write
  // fails — the error is shown, and pressing save again finishes the job.
  const removeProvider = async (gone: string) => {
    const next = { ...draft };
    delete next[gone];
    setDraft(next);
    setSelected(null);
    await persist(next);
  };

  const reload = async () => {
    setReloading(true);
    setError("");
    try {
      const loaded = await getConfig();
      setView(loaded);
      setDraft(toInputs(loaded.model.providers));
      setState("ready");
    } catch (failure) {
      setError(describe(failure));
    } finally {
      setReloading(false);
    }
  };

  if (state === "loading") return <div className={s.note}>正在读取配置…</div>;

  const names = Object.keys(draft).sort();
  const current = selected ? draft[selected] : undefined;
  const held = selected ? view?.model.providers[selected] : undefined;

  return (
    <>
      <div className={s.provHead}>
        <span className={s.provHeadText}>管理模型供应商，配置后可在会话里选择使用。</span>
        <div className={s.provHeadBtns}>
          <button className={s.refreshBtn} onClick={reload} title="重新读取配置">
            <RefreshCw size={14} strokeWidth={2} className={reloading ? s.spin : undefined} />
          </button>
          <button className={s.provAdd} onClick={add}>
            <Plus size={13} strokeWidth={2} />
            添加供应商
          </button>
          <div className={s.presetWrap}>
            <button className={s.provAdd} onClick={openPresets} title="从内置预设带入地址与模型清单">
              <Plug size={13} strokeWidth={2} />
              从预设添加
            </button>
            {presetOpen ? (
              <>
                <div className={s.presetShield} onClick={() => setPresetOpen(false)} />
                <div className={s.presetMenu}>
                  {catalog === null ? <div className={s.presetEmpty}>加载中…</div> : null}
                  {catalog !== null && catalog.length === 0 ? (
                    <div className={s.presetEmpty}>没有可用的预设</div>
                  ) : null}
                  {catalog?.map((preset) => (
                    <button
                      key={preset.slug}
                      className={s.presetItem}
                      onClick={() => pickPreset(preset)}
                    >
                      {preset.name}
                    </button>
                  ))}
                </div>
              </>
            ) : null}
          </div>
        </div>
      </div>

      <div className={s.provLayout}>
        <div className={s.provList}>
          {names.length === 0 ? <div className={s.provEmpty}>还没有供应商</div> : null}
          {names.map((name) => (
            <button
              key={name}
              className={`${s.provItem} ${name === selected ? s.provItemOn : ""}`}
              onClick={() => setSelected(name)}
              title={name}
            >
              <span className={s.provItemName}>{draft[name]?.name || name}</span>
              <span className={`${s.provDot} ${draft[name]?.enabled ? s.provDotOn : ""}`} />
            </button>
          ))}
        </div>

        <div className={s.provEditor}>
          {current && selected ? (
            <ProviderEditor
              name={selected}
              provider={current}
              held={held}
              formats={view?.formats ?? []}
              onChange={(change) => patch(selected, change)}
              onModel={(model) => addModel(selected, model)}
              onModelChange={(model, change) => editModel(selected, model, change)}
              onModelRemove={(id) => removeModel(selected, id)}
              onDelete={() => void removeProvider(selected)}
              onError={setError}
            />
          ) : (
            <div className={s.provEmpty}>选择左侧供应商进行编辑，或者添加一个。</div>
          )}
        </div>
      </div>

      {error ? <div className={s.errText}>{error}</div> : null}

      {/* The save lives under the editor rather than in the toolbar at the top,
          because it is about what is in the editor and nowhere else. It is here
          rather than applied per field because the configuration is one document:
          a page that wrote each field the moment it changed would be a page where
          half a provider exists, and reloading would leave whichever fields had
          not been touched in one state and the rest in another. */}
      <button className={s.primaryBtn} onClick={save} disabled={state === "saving"}>
        {state === "saving" ? "保存中…" : state === "saved" ? "已保存" : "保存"}
      </button>
    </>
  );
}

/**
 * One provider, edited.
 *
 * The head carries the name, whether it is on, and the one destructive thing. The
 * rows under it are the four facts a request needs, and the list under those is
 * what the endpoint offers.
 */
function ProviderEditor({
  name,
  provider,
  held,
  formats,
  onChange,
  onModel,
  onModelChange,
  onModelRemove,
  onDelete,
  onError,
}: {
  name: string;
  provider: ProviderInput;
  held: ProviderView | undefined;
  formats: FormatOption[];
  onChange: (change: Partial<ProviderInput>) => void;
  onModel: (model: ModelInput) => void;
  onModelChange: (model: ModelInput, change: Partial<ModelInput>) => void;
  onModelRemove: (id: string) => void;
  onDelete: () => void;
  onError: (message: string) => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const [formatOpen, setFormatOpen] = useState(false);
  const [showKey, setShowKey] = useState(false);
  const [dialog, setDialog] = useState<{ mode: "new" } | { mode: "edit"; model: ModelInput } | null>(
    null,
  );
  const [probes, setProbes] = useState<Record<string, { ok: boolean; msg: string; busy: boolean }>>(
    {},
  );
  const [listing, setListing] = useState(false);

  const chosenFormat = formats.find((format) => format.value === provider.format);

  // What the server is asked about is the provider as it stands in this editor,
  // saved or not. A probe that could only reach a saved provider would fail on
  // exactly the one somebody is trying to set up.
  const spec = (): ProviderSpec => ({
    name: name || undefined,
    endpoint: provider.endpoint || undefined,
    format: provider.format,
    api_key: provider.api_key || undefined,
    api_key_env: provider.api_key_env || undefined,
  });

  const probe = async (model: ModelInput) => {
    setProbes((current) => ({
      ...current,
      [model.id]: { ok: true, msg: "探测中…", busy: true },
    }));
    try {
      const result = await testProvider(spec(), model.id);
      setProbes((current) => ({
        ...current,
        [model.id]: {
          ok: result.ok,
          msg: result.ok ? `连通（${result.latency_ms ?? 0}ms）` : `失败：${result.error ?? "未知原因"}`,
          busy: false,
        },
      }));
    } catch (failure) {
      setProbes((current) => ({
        ...current,
        [model.id]: { ok: false, msg: `失败：${describe(failure)}`, busy: false },
      }));
    }
  };

  const listFromEndpoint = async () => {
    setListing(true);
    try {
      const listed = await listProviderModels(spec());
      // What the endpoint offers is merged into what is written rather than
      // replacing it: a provider whose endpoint refuses to list anything is still a
      // provider whose models are known.
      onChange({
        models: byId([
          ...(provider.models ?? []),
          ...listed.map((model) => ({ id: model.id })),
        ]),
      });
    } catch (failure) {
      onError(describe(failure));
    } finally {
      setListing(false);
    }
  };

  return (
    <>
      <div className={s.edHead}>
        <input
          className={s.edName}
          value={provider.name ?? ""}
          onChange={(event) => onChange({ name: event.target.value })}
        />
        <Switch
          on={provider.enabled !== false}
          onClick={() => onChange({ enabled: provider.enabled === false })}
          title="启用或停用这个供应商"
        />
        <div className={s.edMenuWrap}>
          <button className={s.iconBtn} onClick={() => setMenuOpen(!menuOpen)} title="更多操作">
            <MoreHorizontal size={16} strokeWidth={2} />
          </button>
          {menuOpen ? (
            <>
              <div className={s.edShield} onClick={() => setMenuOpen(false)} />
              <div className={s.edMenu}>
                <button
                  className={`${s.edMenuItem} ${s.edMenuDanger}`}
                  onClick={() => {
                    setMenuOpen(false);
                    onDelete();
                  }}
                >
                  <Trash2 size={14} strokeWidth={2} />
                  删除
                </button>
              </div>
            </>
          ) : null}
        </div>
      </div>

      <div className={s.edRow}>
        <span className={s.edLabel}>Base URL</span>
        <input
          className={s.edInput}
          value={provider.endpoint}
          onChange={(event) => onChange({ endpoint: event.target.value })}
        />
      </div>

      <div className={s.edRow}>
        <span className={s.edLabel}>API 格式</span>
        <div className={`${s.edMenuWrap} ${s.edSelectWrap}`}>
          <button
            className={s.edSelect}
            onClick={() => setFormatOpen(!formatOpen)}
            title="选择这个端点说哪种话"
          >
            <span className={s.edSelectLabel}>
              {chosenFormat ? `${chosenFormat.label} (${chosenFormat.endpoint})` : provider.format}
            </span>
            <ChevronDown size={14} strokeWidth={2} className={s.edSelectChev} />
          </button>
          {formatOpen ? (
            <>
              <div className={s.edShield} onClick={() => setFormatOpen(false)} />
              <div className={`${s.edMenu} ${s.edMenuWide}`}>
                {formats.map((format) => (
                  <button
                    key={format.value}
                    className={s.edMenuItem}
                    onClick={() => {
                      onChange({ format: format.value });
                      setFormatOpen(false);
                    }}
                  >
                    <span className={s.edMenuLabel}>
                      {format.label} ({format.endpoint})
                    </span>
                    {format.value === provider.format ? (
                      <Check size={14} strokeWidth={2} className={s.edMenuCheck} />
                    ) : null}
                  </button>
                ))}
              </div>
            </>
          ) : null}
        </div>
      </div>

      <div className={s.edRow}>
        <span className={s.edLabel}>API Key</span>
        <div className={s.keyWrap}>
          <input
            className={s.edInput}
            type={showKey ? "text" : "password"}
            value={provider.api_key ?? ""}
            onChange={(event) => onChange({ api_key: event.target.value })}
          />
          <button
            className={s.keyEye}
            onClick={() => setShowKey((shown) => !shown)}
            title={showKey ? "隐藏密钥" : "显示密钥"}
          >
            {showKey ? <EyeOff size={15} strokeWidth={2} /> : <Eye size={15} strokeWidth={2} />}
          </button>
          {held?.api_key_set ? (
            <button
              className={s.iconBtn}
              title="清除已保存的密钥"
              onClick={() => onChange({ api_key: "", clear_api_key: true })}
            >
              <Trash2 size={13} strokeWidth={2} />
            </button>
          ) : null}
        </div>
      </div>

      <div className={s.modelHead}>
        <span className={s.modelHeadTitle}>模型列表</span>
        <div className={s.provHeadBtns}>
          <button
            className={s.miniBtn}
            onClick={listFromEndpoint}
            disabled={listing}
            title="向这个端点问一次它提供哪些模型"
          >
            <RefreshCw size={12} strokeWidth={2} className={listing ? s.spin : undefined} />
            从端点拉取
          </button>
          <button className={s.miniBtn} onClick={() => setDialog({ mode: "new" })}>
            <Plus size={12} strokeWidth={2} />
            添加模型
          </button>
        </div>
      </div>

      <div className={s.modelScroll}>
        {(provider.models ?? []).length === 0 ? (
          <div className={s.provEmpty}>
            还没有模型。点「从端点拉取」问一次，或点「添加模型」手写一个。
          </div>
        ) : null}
        {(provider.models ?? []).map((model) => {
          const said = probes[model.id];
          return (
            <div key={model.id} className={`${s.modelRow} ${model.disabled ? s.modelRowOff : ""}`}>
              <div className={s.modelLeft}>
                <span className={s.modelId} title={model.id}>
                  {model.id}
                </span>
                <span
                  className={s.modelMeta}
                  title={
                    model.declared_window
                      ? "这个模型自己声明的上下文窗口"
                      : `没声明，按默认 ${DEFAULT_CTX_WINDOW} 计算`
                  }
                >
                  ctx {fmtTokens(model.context_window ?? DEFAULT_CTX_WINDOW)}
                </span>
                <span
                  className={s.modelMeta}
                  title={
                    model.declared_output
                      ? "这个模型自己声明的单次输出上限"
                      : `没声明，按默认 ${DEFAULT_MAX_OUT} 计算`
                  }
                >
                  out {fmtTokens(model.max_output ?? DEFAULT_MAX_OUT)}
                </span>
                <span className={`${s.modelMeta} ${s.modelTag}`}>
                  {model.vision ? "视觉" : "文本"}
                </span>
                {(model.thinking ?? []).length > 0 ? (
                  <span
                    className={s.modelMeta}
                    title="这个模型支持的思考强度，会话里的思考选项由它派生"
                  >
                    思考 {(model.thinking ?? []).map((level) => THINKING_LABELS[level] ?? level).join("/")}
                  </span>
                ) : null}
              </div>
              <div className={s.modelRight}>
                {said ? (
                  <span className={said.ok ? s.testOk : s.testMsg} title={said.msg}>
                    {said.busy ? "探测中…" : said.msg}
                  </span>
                ) : null}
                <button
                  className={s.iconBtn}
                  title="向这个端点发一次真实请求，看看它通不通"
                  onClick={() => probe(model)}
                  disabled={model.disabled}
                >
                  <Plug size={13} strokeWidth={2} />
                </button>
                <button
                  className={s.iconBtn}
                  title="编辑"
                  onClick={() => setDialog({ mode: "edit", model })}
                >
                  <Pencil size={13} strokeWidth={2} />
                </button>
                <button
                  className={s.iconBtn}
                  title="删除"
                  onClick={() => onModelRemove(model.id)}
                >
                  <Trash2 size={13} strokeWidth={2} />
                </button>
                <Switch
                  on={!model.disabled}
                  onClick={() => onModelChange(model, { disabled: !model.disabled })}
                  title="启用或停用这个模型"
                />
              </div>
            </div>
          );
        })}
      </div>

      {dialog ? (
        <ModelDialog
          initial={dialog.mode === "edit" ? dialog.model : null}
          onCancel={() => setDialog(null)}
          onSave={(model) => {
            if (dialog.mode === "new") onModel(model);
            else onModelChange(dialog.model, model);
            setDialog(null);
          }}
        />
      ) : null}
    </>
  );
}

/**
 * One model, added or edited.
 *
 * A dialog rather than four more rows in the editor: a model has four facts about
 * it, and somebody editing one wants to see all of them at once, which an editor
 * that grows a row at a time does not give them.
 */
function ModelDialog({
  initial,
  onSave,
  onCancel,
}: {
  initial: ModelInput | null;
  onSave: (model: ModelInput) => void;
  onCancel: () => void;
}) {
  const [id, setId] = useState(initial?.id ?? "");
  const [ctx, setCtx] = useState(initial?.context_window?.toString() ?? "");
  const [maxOut, setMaxOut] = useState(initial?.max_output?.toString() ?? "");
  const [vision, setVision] = useState(initial?.vision ?? false);
  const [thinking, setThinking] = useState<string[]>(initial?.thinking ?? []);
  const [disabled, setDisabled] = useState(initial?.disabled ?? false);

  /** A field left empty is "not declared", not "none": the two are different facts
   *  and only one of them can be written down. */
  const declared = (value: string) => {
    const cleaned = value.replace(/[,\s]/g, "");
    if (!cleaned) return { declared: false, number: 0 };
    const parsed = Number(cleaned);
    return Number.isFinite(parsed) && parsed > 0
      ? { declared: true, number: Math.round(parsed) }
      : { declared: false, number: 0 };
  };

  const window = declared(ctx);
  const output = declared(maxOut);

  const save = () => {
    const name = id.trim();
    if (!name) return;
    onSave({
      id: name,
      context_window: window.number,
      max_output: output.number,
      declared_window: window.declared,
      declared_output: output.declared,
      vision,
      thinking,
      disabled,
    });
  };

  const enter = (event: React.KeyboardEvent) => {
    if (event.key === "Enter") save();
  };

  return (
    <div className={s.dlgOverlay} onClick={onCancel}>
      <div className={s.dlg} onClick={(event) => event.stopPropagation()}>
        <div className={s.dlgTitle}>{initial ? "编辑模型" : "添加模型"}</div>

        <label className={s.fldLabel}>模型 id</label>
        <input
          className={s.fldInput}
          autoFocus
          value={id}
          placeholder="qwen3:8b"
          onChange={(event) => setId(event.target.value)}
          onKeyDown={enter}
        />

        <label className={s.fldLabel}>上下文窗口（tokens）</label>
        <input
          className={s.fldInput}
          value={ctx}
          placeholder={String(DEFAULT_CTX_WINDOW)}
          onChange={(event) => setCtx(event.target.value)}
          onKeyDown={enter}
        />
        <div className={s.fldHint}>
          留空表示这个模型没有声明，按 {DEFAULT_CTX_WINDOW} 计算。会话压缩就是按它决定的。
        </div>

        <label className={s.fldLabel}>最大输出 token 限制</label>
        <input
          className={s.fldInput}
          value={maxOut}
          placeholder={String(DEFAULT_MAX_OUT)}
          onChange={(event) => setMaxOut(event.target.value)}
          onKeyDown={enter}
        />
        <div className={s.fldHint}>留空按 {DEFAULT_MAX_OUT} 计算。</div>

        <label className={s.fldLabel}>能力</label>
        <div className={s.dlgThinking}>
          <button
            type="button"
            className={`${s.dlgThinkBtn} ${vision ? s.dlgThinkOn : ""}`}
            onClick={() => setVision((current) => !current)}
          >
            接受图片
          </button>
          <button
            type="button"
            className={`${s.dlgThinkBtn} ${disabled ? s.dlgThinkOn : ""}`}
            onClick={() => setDisabled((current) => !current)}
          >
            停用
          </button>
        </div>

        <label className={s.fldLabel}>思考强度档位</label>
        <div className={s.dlgThinking}>
          {THINKING_LEVELS.map((level) => (
            <button
              key={level}
              type="button"
              className={`${s.dlgThinkBtn} ${thinking.includes(level) ? s.dlgThinkOn : ""}`}
              onClick={() =>
                setThinking((current) =>
                  current.includes(level) ? current.filter((x) => x !== level) : [...current, level],
                )
              }
            >
              {THINKING_LABELS[level]}
            </button>
          ))}
          <span className={s.dlgThinkNote}>
            一个都不勾选 = 这个模型没有思考控制，会话里的思考选项对它显示「模型默认」。
          </span>
        </div>

        <div className={s.dlgBtns}>
          <button className={s.dlgBtn} onClick={onCancel}>
            取消
          </button>
          <button className={`${s.dlgBtn} ${s.dlgBtnPrimary}`} disabled={!id.trim()} onClick={save}>
            保存
          </button>
        </div>
      </div>
    </div>
  );
}

/** The assumed figures, spelled here rather than in three places in the markup. */
const DEFAULT_CTX_WINDOW = 131072;
const DEFAULT_MAX_OUT = 16384;

function byId(models: ModelInput[]): ModelInput[] {
  const seen = new Map<string, ModelInput>();
  for (const model of models) {
    if (model.id) seen.set(model.id, model);
  }
  return [...seen.values()].sort((left, right) => left.id.localeCompare(right.id));
}

/** The stored providers as the page holds them while they are being edited. */
function toInputs(providers: Record<string, ProviderView>): Record<string, ProviderInput> {
  const inputs: Record<string, ProviderInput> = {};
  for (const [name, provider] of Object.entries(providers)) {
    inputs[name] = {
      name: provider.name ?? name,
      enabled: provider.enabled,
      endpoint: provider.endpoint,
      format: provider.format,
      api_key_env: provider.api_key_env ?? "",
      // The key comes back with the configuration, so the field starts showing what
      // is stored. An empty field then means a key that has not been typed yet,
      // which is one thing, rather than two things at once.
      api_key: provider.api_key ?? "",
      models: (provider.models ?? []).map(toInput),
      headers: provider.headers,
      server_state: provider.server_state,
      thinking: provider.thinking,
    };
  }
  return inputs;
}

function toInput(model: ModelView): ModelInput {
  return {
    id: model.id,
    context_window: model.context_window,
    max_output: model.max_output,
    declared_window: model.declared_window,
    declared_output: model.declared_output,
    vision: model.vision,
    thinking: model.thinking,
    disabled: model.disabled,
  };
}

/** What a failure says, in the words it arrived in. */
function describe(failure: unknown): string {
  if (failure instanceof ApiError) return failure.message;
  if (failure instanceof Error) return failure.message;
  return String(failure);
}

function Hotkeys() {
  return (
    <>
      <KV label="Enter" value="发送消息" />
      <KV label="Shift + Enter" value="消息内换行" />
      <KV label="Ctrl / Cmd + W" value="关闭当前标签页" />
    </>
  );
}

function Mcp() {
  return null;
}

function Stats() {
  const sessions = useWork((state) => state.sessions.length);
  const tabs = useWork((state) => state.tabs.length);
  const running = useWork(
    (state) => state.sessions.filter((session) => session.state === "running").length,
  );
  return (
    <>
      {/* The three numbers below are counted in this window and are true as far as they
          go. The ones the server owns are not estimated here. */}
      <KV label="会话" value={String(sessions)} />
      <KV label="正在运行" value={String(running)} />
      <KV label="打开的标签页" value={String(tabs)} />
    </>
  );
}

/** A label on the left and a value on the right. */
function KV({ label, value }: { label: string; value: string }) {
  return (
    <div className={s.kvRow}>
      <span className={s.label}>{label}</span>
      <span className={s.value}>{value}</span>
    </div>
  );
}
