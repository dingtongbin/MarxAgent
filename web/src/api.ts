/**
 * The one place the window talks to the server.
 *
 * It is a file of its own, and it is small, because everything that could be scattered
 * across call sites is here instead: where the server is, how the token travels, and
 * what happens when the server answers with something unusable.
 *
 * Nothing else in the interface builds a URL. A fetch with a host written into it is a
 * fetch that has to be found and changed when the address changes, and the person who
 * changes it is not looking for it.
 */

/** Thrown when the server answered, and the answer was not usable. */
export class ApiError extends Error {
  readonly status: number;
  readonly endpoint: string;

  constructor(endpoint: string, status: number, detail: string) {
    super(detail || `${endpoint} 返回 ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.endpoint = endpoint;
  }
}

/**
 * Where the server is.
 *
 * The interface is served from the same origin as the API, in development and in a
 * packaged build alike, so a relative path is the correct answer and an absolute one is
 * a chance to be wrong about which machine this is. The environment variable exists for
 * the case where they are not the same, and it is read in one place.
 */
function baseUrl(): string {
  const configured = import.meta.env.VITE_API_BASE;
  return typeof configured === "string" && configured.length > 0
    ? configured.replace(/\/+$/, "")
    : "";
}

/** The bootstrap endpoint, spelled once so a change to it is one change. */
const bootstrapPath = "/v1/bootstrap";

/** The token as it is already known, without asking anybody. */
function stored(): string {
  const params = new URLSearchParams(window.location.hash.replace(/^#/, ""));
  const fromFragment = params.get("token");
  if (fromFragment) {
    document.cookie = `marxagent_token=${encodeURIComponent(fromFragment)}; path=/; SameSite=Strict`;
    // It leaves the address bar once it is in a cookie, so it is not left where a
    // screenshot or a shared link would carry it.
    history.replaceState(null, "", window.location.pathname + window.location.search);
    return fromFragment;
  }
  const match = document.cookie.match(/(?:^|;\s*)marxagent_token=([^;]+)/);
  return match ? decodeURIComponent(match[1]) : "";
}

/**
 * The token, from the page's own link, and failing that asked for once.
 *
 * The fragment because a browser never sends it to the server, so it never lands in an
 * access log. The cookie because a cookie travels with each request by itself, and the
 * event stream in particular cannot attach a header of its own.
 *
 * And the request, because in development the page is served by the dev server on a
 * different port from the one that generated the token — so the link carrying the token
 * points at the built interface rather than at the one being worked on. Without this,
 * opening the development page means copying a token out of the server's output and
 * pasting it into another address, every single time something is started.
 *
 * The ask is remembered in a variable as well as in the cookie, so two components
 * mounting at the same moment do not both ask for it.
 */
let asked: Promise<string> | null = null;

export function token(): Promise<string> {
  const existing = stored();
  if (existing) return Promise.resolve(existing);
  asked ??= fetch(`${baseUrl()}${bootstrapPath}`)
    .then(async (response) => {
      if (!response.ok) throw new Error(`这个服务没有给出 token（${response.status}）`);
      const body = (await response.json()) as { token?: string };
      if (!body.token) throw new Error("服务没有返回 token");
      document.cookie = `marxagent_token=${encodeURIComponent(body.token)}; path=/; SameSite=Strict`;
      return body.token;
    })
    .finally(() => {
      // Dropped whatever happened, so a page that was open before the server came up
      // can ask again once it is up, rather than being stuck with the first failure for
      // the life of the page.
      asked = null;
    });
  return asked;
}

/**
 * One request.
 *
 * There is no retry and no backoff. Every call in this file is a question whose answer
 * changes what is on screen, and a caller that retries a question it has already been
 * told the answer to would be showing a person a directory twice.
 *
 * A body is sent as JSON and the answer is read as JSON. Both halves matter: a
 * settings page that saved a configuration as form encoding would be a page
 * talking a dialect the server does not speak, and the failure would show up as a
 * field that silently did not save.
 */
/**
 * How long one request may take before it is called a failure.
 *
 * A request that never answers is worse than one that answers badly: a bad answer is
 * something a person can read and act on, and a request that hangs is a spinner that
 * never stops and says nothing about why. Every call this file makes is a question
 * whose answer arrives in well under a second on a local machine — a directory
 * listing, a configuration save, a provider's model list — so a minute is far past
 * anything legitimate, and anything still waiting at that point is a request whose
 * server is gone or wedged.
 *
 * The event stream is not one of these calls: it holds its connection open for as long
 * as the conversation lasts, which is the whole point of it, and it never goes through
 * here.
 */
const requestBudget = 60_000;

export async function request<T>(method: string, endpoint: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Authorization: `Bearer ${await token()}` };
  const init: RequestInit = { method, headers };
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  // The timer is cleared whichever way this ends, so a request that succeeds in five
  // milliseconds does not leave one of these running for the rest of the window.
  const budget = new AbortController();
  const expiry = setTimeout(() => budget.abort(), requestBudget);
  init.signal = budget.signal;
  let response: Response;
  try {
    response = await fetch(`${baseUrl()}${endpoint}`, init);
  } catch (cause) {
    if (budget.signal.aborted) {
      throw new ApiError(endpoint, 0, `${endpoint} 没有回应（超过 ${requestBudget / 1000} 秒）`);
    }
    throw cause;
  } finally {
    clearTimeout(expiry);
  }

  if (response.status === 401) {
    // The token this page is holding is not one the server recognises.
    //
    // That is not a rare case, it is the normal case after a restart: the server makes
    // a new token every time it starts, and a page that was open across the restart is
    // holding the previous one. Without this the page would be broken until somebody
    // cleared their cookies by hand, which is a thing no person is going to think to do
    // or would know how to do.
    //
    // So the stale one is dropped and a fresh one asked for, once. Not twice: a server
    // that has just refused a freshly issued token is not going to accept the next one
    // either, and a page that keeps asking is a page that keeps the server busy.
    forget();
    // The content type goes across with the body. Without it a retried POST arrives as
    // a body the server cannot read, and the answer to "your token was stale" becomes
    // "your request made no sense" — a second failure that looks nothing like the first
    // and is the reason this path was rewritten rather than copied.
    const retryHeaders: Record<string, string> = { Authorization: `Bearer ${await token()}` };
    const retryInit: RequestInit = { method, headers: retryHeaders };
    if (body !== undefined) {
      retryHeaders["Content-Type"] = "application/json";
      retryInit.body = JSON.stringify(body);
    }
    const again = new AbortController();
    const expiry = setTimeout(() => again.abort(), requestBudget);
    init.signal = again.signal;
    let retry: Response;
    try {
      retry = await fetch(`${baseUrl()}${endpoint}`, retryInit);
    } catch (cause) {
      if (again.signal.aborted) {
        throw new ApiError(endpoint, 0, `${endpoint} 没有回应（超过 ${requestBudget / 1000} 秒）`);
      }
      throw cause;
    } finally {
      clearTimeout(expiry);
    }
    if (!retry.ok) {
      const detail = await retry.text().catch(() => "");
      throw new ApiError(endpoint, retry.status, detail.slice(0, 400));
    }
    return (await retry.json()) as T;
  }

  if (!response.ok) {
    const detail = await response.text().catch(() => "");
    throw new ApiError(endpoint, response.status, detail.slice(0, 400));
  }
  if (response.status === 204) return undefined as T;
  const text = await response.text();
  // A body that is not JSON is a fault somewhere between here and the server, and
  // handing it back as an object would turn a page that could say what went wrong
  // into a page that fails on a property of undefined.
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError(endpoint, response.status, text.slice(0, 400) || "服务没有返回可解析的内容");
  }
}

/**
 * Drop the token this page is holding.
 *
 * The cookie is the only place it is kept, so removing that is the whole of it. The
 * in-flight ask is dropped with it so that the retry asks the server rather than being
 * handed the value that was just refused.
 */
function forget(): void {
  document.cookie = "marxagent_token=; path=/; Max-Age=0; SameSite=Strict";
  asked = null;
}

/**
 * One folder in a directory listing.
 *
 * There is no size, because only folders are ever listed and a folder has no size that
 * means anything. `dir` is still here rather than assumed: a payload that says what it
 * is does not depend on the reader having read the server's reasons.
 */
export type DirEntry = {
  name: string;
  /**
   * Where this folder is, whole.
   *
   * Taken from the server rather than put together here, because a path belongs to the
   * machine that has it: joining a child onto its parent needs the separator of that
   * machine, and a caller that guesses produces `C:\Users30978` out of two halves that
   * were each correct.
   */
  path: string;
  dir: boolean;
  modified?: string;
};

/** What a directory answers with. */
export type DirListing = {
  roots: boolean;
  path: string;
  parent: string | null;
  entries: DirEntry[];
  error?: string;
};

/**
 * The folders in a directory.
 *
 * An empty path asks for the top of the machine, which is the drive letters on Windows
 * and the root everywhere else. The server decides that rather than this file, because
 * "the top" is the one question whose answer is a fact about the machine rather than
 * about the interface.
 *
 * Only folders come back. The question this answers is which folder, and a file is not
 * a candidate: picking one would be a path with no working directory in it.
 */
export function browseDir(path: string): Promise<DirListing> {
  return request<DirListing>("GET", `/v1/fs/browse?path=${encodeURIComponent(path)}`);
}

/**
 * What git sees in one project.
 *
 * A read-only answer: the working tree's changes and the recent history, as
 * git itself writes them. A project that is not a repository answers with its
 * error rather than with nothing.
 */
export type GitView = {
  root: string;
  branch?: string;
  status?: string;
  log?: string;
  error?: string;
};

export function gitStatus(path: string): Promise<GitView> {
  return request<GitView>("GET", `/v1/fs/git?path=${encodeURIComponent(path)}`);
}

/**
 * What a conversation is doing. The four are the ones the ledger records, so the
 * interface never has to invent a fifth.
 */
export type SessionState = "idle" | "running" | "checkpointed" | "degraded";

/** A conversation as the task menu sees it. */
export type SessionInfo = {
  id: string;
  title?: string;
  state: SessionState;
  created_at?: string;
  updated_at?: string;
  /** The directory it is about. Absent means a conversation with no folder. */
  project?: string;
  /** The reach its next turn runs at: build, plan or full access. Chat sessions have none. */
  execution?: string;
  model?: string;
  mode?: string;
  archived?: boolean;
};

/**
 * The conversations on this machine.
 *
 * Read at start and after anything that opens or removes one, because the menu is
 * the only place that knows what exists.
 */
export async function listSessions(): Promise<SessionInfo[]> {
  const body = await request<{ sessions: SessionInfo[] }>("GET", "/v1/sessions");
  return body.sessions ?? [];
}

/* --- Conversations --- */

/** What a conversation is made of. */
export type { ChatMessage, ContentBlock, StreamEvent } from "@/types";

import type { ChatMessage } from "@/types";

/** A conversation as the task menu knows it, plus its messages when asked whole. */
export type Conversation = {
  info: {
    id: string;
    title?: string;
    state: SessionState;
    mode?: string;
    created_at?: string;
    updated_at?: string;
    project?: string;
    model?: string;
    archived?: boolean;
  };
  messages: ChatMessage[];
};

/**
 * Open a conversation.
 *
 * The model is the one to use for it: whatever the window chose, and failing that
 * whatever the configuration says. It is sent because the model a conversation
 * runs on is part of the conversation, not part of the window that happened to
 * open it.
 */
export function createSession(input: {
  project?: string;
  title?: string;
  model?: string;
}): Promise<Conversation["info"]> {
  return request<Conversation["info"]>("POST", "/v1/sessions", {
    project: input.project || undefined,
    title: input.title || undefined,
    model: input.model || undefined,
  });
}

/** One conversation with everything said in it. */
export function getSession(id: string): Promise<Conversation> {
  return request<Conversation>("GET", `/v1/sessions/${encodeURIComponent(id)}`);
}

/**
 * Send a message.
 *
 * Accepted rather than finished: the turn is running and what it produces arrives
 * on the stream. A window that waited for the answer here would be holding a
 * request open for as long as a model takes to answer, which is a request a proxy
 * will cut off.
 *
 * The execution level travels with the turn rather than living on the
 * conversation, because the level a turn runs at is what the composer said when
 * the turn was sent: empty means the level the conversation has been at, which
 * is build until a turn says otherwise.
 */
export function postMessage(
  id: string,
  text: string,
  model?: string,
  execution?: string,
): Promise<SessionInfo> {
  return request<SessionInfo>("POST", `/v1/sessions/${encodeURIComponent(id)}/messages`, {
    text,
    model: model || undefined,
    execution: execution || undefined,
  });
}

/** Stop a turn in progress. Answering this while nothing is running is not an error. */
export function interruptSession(id: string): Promise<{ interrupted: boolean }> {
  return request<{ interrupted: boolean }>(
    "POST",
    `/v1/sessions/${encodeURIComponent(id)}/interrupt`,
  );
}

/**
 * Where this conversation's events come from.
 *
 * An EventSource and not a fetch: the browser attaches the token cookie to this by
 * itself, and a stream is held open for as long as the conversation is, which is
 * something a fetch that has to be reissued every time is bad at.
 */
export function eventStreamUrl(sessionId: string): string {
  return `${baseUrl()}/events?session=${encodeURIComponent(sessionId)}`;
}

/* --- The tab strip --- */

/** One tab in a window's strip. */
export type Tab = { id: string; kind: string };

/** The whole tab strip. */
export type TabTree = { tabs: Tab[]; active?: string; updated_at?: string };

/**
 * What this machine has open.
 *
 * There is no window identifier and nothing stored on the browser to hold one. A
 * strip kept per window would need a value on the browser to tell one window from
 * another, and that is the work's state living somewhere it does not belong: a
 * browser tidy-up takes it, another machine never had it, and the same window
 * would come back to different tabs. So the strip belongs to the installation and
 * this window is simply reading it.
 *
 * Nothing else in this file writes to the browser either. The token goes in a
 * cookie because a cookie travels with a request by itself; that is transport, not
 * a copy of anything anybody would miss.
 */
export function getWorkspace(): Promise<TabTree> {
  return request<TabTree>("GET", "/v1/workspace");
}

/**
 * Tell the server what this window has open.
 *
 * The answer is what was stored rather than what was sent, because the store
 * corrects two things: a tab listed twice, and a strip whose active tab is not in
 * it. Writing back what was kept is what stops the window from showing a duplicate
 * until the next reload.
 */
export function putWorkspace(tree: TabTree): Promise<TabTree> {
  return request<TabTree>("PUT", "/v1/workspace", tree);
}

/* --- Model configuration --- */

/** One of the three API formats, as the settings page offers it. */
export type FormatOption = { value: string; label: string; endpoint: string };

/**
 * One model as the settings page is shown it.
 *
 * The two declared flags are why this is not the same shape as what the page
 * sends: a window and an output limit that were never filled in come back as the
 * assumed figures, so a list can be drawn without every row having to know the
 * defaults, and the flags say which of those numbers are real.
 */
export type ModelView = {
  id: string;
  context_window: number;
  max_output: number;
  vision: boolean;
  thinking?: string[];
  disabled: boolean;
  declared_window: boolean;
  declared_output: boolean;
};

/**
 * One provider as the settings page is shown it.
 *
 * The key comes back as it is written down, so a person editing a provider can see
 * what is there without going to the file. It travels over the same loopback
 * connection behind the same token that everything else does, to the same window
 * they are looking at, and the key is already in a file on this machine that they
 * can read.
 */
export type ProviderView = {
  name?: string;
  enabled: boolean;
  endpoint: string;
  format: string;
  api_key_env?: string;
  api_key?: string;
  api_key_set: boolean;
  api_key_present_from_env: boolean;
  models: ModelView[];
  headers?: Record<string, string>;
  server_state?: boolean;
  thinking?: { enabled: boolean; budget_tokens?: number; reasoning_effort?: string };
};

/** What the settings page reads. */
export type ConfigView = {
  version: number;
  model: { default: string; providers: Record<string, ProviderView> };
  formats: FormatOption[];
  global_path: string;
  project_path?: string;
};

/** One model as the settings page sends it. */
export type ModelInput = {
  id: string;
  context_window?: number;
  max_output?: number;
  vision?: boolean;
  thinking?: string[];
  disabled?: boolean;
  declared_window?: boolean;
  declared_output?: boolean;
};

/** One provider as the settings page sends it. */
export type ProviderInput = {
  name?: string;
  enabled?: boolean;
  endpoint: string;
  format: string;
  api_key_env?: string;
  /** Sent only when it is being changed. An absent field keeps the stored one. */
  api_key?: string;
  /** The one operation a value cannot express. */
  clear_api_key?: boolean;
  models?: ModelInput[];
  headers?: Record<string, string>;
  server_state?: boolean;
  thinking?: { enabled: boolean; budget_tokens?: number; reasoning_effort?: string };
};

/** What the settings page saves. */
export type ConfigUpdate = {
  model: { default: string; providers: Record<string, ProviderInput> };
};

export function getConfig(): Promise<ConfigView> {
  return request<ConfigView>("GET", "/v1/config");
}

/**
 * Save the configuration and get back what was stored.
 *
 * The answer is the saved configuration rather than an empty object, so a page that
 * saved something can show what was written without asking again — and can show the
 * key it is not allowed to see as "set" because the server said so.
 */
export function putConfig(update: ConfigUpdate): Promise<ConfigView> {
  return request<ConfigView>("PUT", "/v1/config", update);
}

/**
 * A provider this build knows about, offered rather than typed.
 *
 * It carries no models. The list a preset would have given out is a list somebody
 * typed once and never checked, and a stale name in it offers a model the endpoint
 * refuses. The models come from the endpoint when they are asked for.
 */
export type Preset = {
  name: string;
  slug: string;
  endpoint: string;
  format: string;
  api_key_env?: string;
};

/**
 * The providers this build knows about.
 *
 * Asked for when the menu first opens rather than when the page loads, because it
 * is a list nobody looks at until they want it and it is the one thing here that
 * can change without the file changing.
 */
export async function getProviderCatalog(): Promise<Preset[]> {
  const body = await request<{ catalog: Preset[] }>("GET", "/v1/providers");
  return body.catalog ?? [];
}

/** One model an endpoint offers. */
export type Model = { id: string; display_name?: string };

/**
 * A provider as one request carries it.
 *
 * Whole rather than by name, because the two things a person does with a provider
 * they have just typed — asking it what models it has, and asking whether it
 * answers at all — happen before the save, every time. Sending only the name would
 * make both of them fail on exactly the provider somebody is trying to set up.
 */
export type ProviderSpec = {
  name?: string;
  endpoint?: string;
  format?: string;
  api_key?: string;
  api_key_env?: string;
  headers?: Record<string, string>;
};

/**
 * Ask a provider which models it offers.
 *
 * A GET that reaches out to a third party, so the answer can be a refusal, and the
 * error is the endpoint's own words rather than a generic one: "the variable
 * MARXAGENT_KEY is not set" and "the request timed out" are different problems and
 * a page that showed the same message for both would be sending somebody to the
 * wrong place.
 */
export async function listProviderModels(spec: ProviderSpec): Promise<Model[]> {
  const body = await request<{ models: Model[] }>("POST", "/v1/providers/models", spec);
  return body.models ?? [];
}

/**
 * What a probe found.
 *
 * A failure is an answer here rather than a fault, so it arrives as `ok: false`
 * with a reason rather than as an error: the page asked whether a key works and
 * "no, and here is why" is the answer, not a failure of the page.
 */
export type ProbeResult = {
  provider: string;
  model: string;
  ok: boolean;
  latency_ms?: number;
  context_window?: number;
  error?: string;
};

export function testProvider(spec: ProviderSpec, model: string): Promise<ProbeResult> {
  return request<ProbeResult>("POST", "/v1/providers/test", { ...spec, model });
}
