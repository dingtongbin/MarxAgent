import { create } from "zustand";
import {
  ApiError,
  eventStreamUrl,
  getSession,
  postMessage,
  interruptSession,
  type ChatMessage,
  type ContentBlock,
  type StreamEvent,
} from "@/api";
import { useWork, type SessionInfo } from "@/store/work";

/**
 * The conversations this window is watching, and what is happening in them.
 *
 * The messages live here rather than inside a component because they outlive the
 * panel showing them: a tab is mounted once, and a message arrives as a stream that
 * keeps going while the tab is in the background and has to be there when it comes
 * back.
 *
 * The stream is the source of what is happening, not the answer to a request. A send
 * is accepted and returns before a model has said anything, so anything that waited
 * for the request to know what happened would know nothing.
 */

/** One conversation being watched. */
type Watch = {
  messages: ChatMessage[];
  /**
   * The message being streamed into, if one is.
   *
   * It is what makes a turn that is still happening different from one that has
   * finished: the answer grows in place, and a person watching can see which message
   * is still moving rather than watching a spinner and not knowing what it belongs to.
   */
  streamingId: string | null;
  running: boolean;
  error: string;
  /**
   * The turn is waiting through a pause it may come back from.
   *
   * A model call that failed on a dropped connection is not over: the turn
   * announces each retry it is about to make, and what is here is the line it
   * announced. It is cleared by the next event of any kind, because progress
   * is the proof the wait ended, and by the turn ending, whichever way that
   * goes.
   */
  waiting: string;
  /**
   * The conversation is not there.
   *
   * A tab can outlive what it points at — the server was restarted against a different
   * data directory, or the conversation was removed. That is worth saying as itself,
   * because the alternative is a tab that can never load and looks like a page that is
   * still thinking.
   */
  gone: boolean;
  loaded: boolean;
};

type ConversationState = {
  watches: Record<string, Watch>;
  /** Read a conversation and open its stream. Safe to call again at any time. */
  load: (sessionId: string) => Promise<void>;
  send: (sessionId: string, text: string, model?: string, execution?: string) => Promise<void>;
  interrupt: (sessionId: string) => Promise<void>;
  /** Forget a conversation, when its tab is closed for good. */
  forget: (sessionId: string) => void;
};

const empty: Watch = {
  messages: [],
  streamingId: null,
  running: false,
  error: "",
  waiting: "",
  gone: false,
  loaded: false,
};

/**
 * How a refusal is said.
 *
 * A server's own words are written for a log: a person reading this has to be told
 * what to do, not what was rejected. So the two refusals a window can actually cause
 * are given the next step, and everything else is passed on as it arrived rather than
 * flattened into something vaguer than the truth.
 */
function refusal(failure: unknown): string {
  const detail = failure instanceof Error ? failure.message : String(failure);
  if (detail.includes("no model is configured")) {
    return "还没有选择模型。在输入框上方选一个模型，再发这条消息。";
  }
  if (detail.includes("already running")) {
    return "这个会话已经有一轮在跑了。等它说完，或者先停掉它。";
  }
  return detail;
}

/**
 * How long a message waits for its stream to be open.
 *
 * A turn is not sent into a conversation this window cannot hear, because the first
 * tokens of an answer arrive before the request that asked for them has finished and
 * a window that was not attached yet is not given them again: the stream carries what
 * happens from the moment it is opened, and only a window that already held a
 * position is sent the events that came after it.
 *
 * The wait is bounded rather than open-ended. A server that is not answering should
 * not stop somebody from sending, and the message is recovered over the API when the
 * connection is there — a turn sent blind is better than no turn.
 */
const openBudget = 3000;

/** One EventSource per conversation, and when it was open. */
const streams = new Map<string, EventSource>();
const opened = new Map<string, Promise<void>>();

export const useConversation = create<ConversationState>((set) => {
  const watch = (sessionId: string): Watch =>
    useConversation.getState().watches[sessionId] ?? empty;

  const patch = (sessionId: string, change: Partial<Watch>) =>
    set((state) => ({
      watches: { ...state.watches, [sessionId]: { ...watch(sessionId), ...change } },
    }));

  const listen = (sessionId: string): Promise<void> => {
    const already = opened.get(sessionId);
    if (already) return already;

    const source = new EventSource(eventStreamUrl(sessionId));
    streams.set(sessionId, source);

    const ready = new Promise<void>((resolve) => {
      let settled = false;
      let timer = 0;
      const finish = () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve();
      };
      source.onopen = finish;
      source.onerror = () => {
        // The browser reconnects by itself, holding the position it last saw, and the
        // stream server replays what it missed. Ending it here would throw that away,
        // so this only says the wait is over: a stream that never opened must not
        // hold a turn hostage.
        finish();
      };
      timer = setTimeout(finish, openBudget) as unknown as number;
    });
    opened.set(sessionId, ready);

    source.onmessage = (event) => {
      let parsed: StreamEvent;
      try {
        parsed = JSON.parse(event.data) as StreamEvent;
      } catch {
        // A frame that is not an event is a frame about the transport. Nothing about
        // it can be put in the conversation, and it is not a reason to end the stream:
        // the next one may be an answer.
        return;
      }
      apply(sessionId, parsed, set, patch);
    };

    return ready;
  };

  return {
    watches: {},

    load: async (sessionId) => {
      if (!sessionId) return;
      const stream = listen(sessionId);
      if (!watch(sessionId).loaded) {
        try {
          const conversation = await getSession(sessionId);
          patch(sessionId, {
            // The history comes in the same shape the stream tells it: the tool's
            // answers as messages of their own. The same folding that the live stream
            // applies is applied here, so a window opened later reads the same
            // conversation as the one that watched it happen.
            messages: foldToolMessages(conversation.messages ?? []),
            loaded: true,
          });
        } catch (failure) {
          patch(sessionId, {
            loaded: true,
            // A conversation that is not there is a different thing from one that
            // could not be reached, and only one of them is worth retrying.
            gone: failure instanceof ApiError && failure.status === 404,
            error: failure instanceof ApiError && failure.status === 404 ? "" : refusal(failure),
          });
        }
      }
      await stream;
    },

    send: async (sessionId, text, model, execution) => {
      const trimmed = text.trim();
      if (!trimmed) return;
      // The turn is announced before anything is asked of the network. Waiting for the
      // stream to open and the request to be accepted first would mean the window sits
      // unchanged for as long as that takes, on a question somebody has already asked:
      // the gap between pressing send and seeing anything happen is the gap in which
      // the window looks broken.
      patch(sessionId, { running: true, error: "" });
      // The window is attached to this conversation before the message goes to it.
      // Sending first and listening after would lose whatever the model said first,
      // with nothing to indicate that anything was missing.
      await listen(sessionId);
      try {
        const info = await postMessage(sessionId, trimmed, model, execution);
        // The session now has a model, and the turn has a state; the task menu reads
        // it from here rather than waiting for somebody to reload the list.
        if (info?.id) useWork.getState().upsertSession(info);
      } catch (failure) {
        patch(sessionId, { running: false, error: refusal(failure) });
      }
    },

    interrupt: async (sessionId) => {
      try {
        await interruptSession(sessionId);
      } catch (failure) {
        patch(sessionId, { error: refusal(failure) });
      }
    },

    forget: (sessionId) => {
      streams.get(sessionId)?.close();
      streams.delete(sessionId);
      opened.delete(sessionId);
      set((state) => {
        const watches = { ...state.watches };
        delete watches[sessionId];
        return { watches };
      });
    },
  };
});

/**
 * Fold one event into what the window is showing.
 *
 * The pieces are: the conversation's own state, a message arriving whole, a piece of
 * one arriving, the stream finishing, a turn failing, and a stream telling this
 * window it can no longer be trusted to have everything.
 */
function apply(
  sessionId: string,
  event: StreamEvent,
  set: (change: (state: ConversationState) => Partial<ConversationState>) => void,
  patch: (sessionId: string, change: Partial<Watch>) => void,
) {
  switch (event.type) {
    case "waiting": {
      // The turn is waiting through a pause it may come back from. The
      // announcement names the message it is retrying when the stream died
      // half drawn: the half is cleared, because the retry starts that
      // message over, and keeping the pieces would grow them into a double
      // answer.
      const payload = event.data as
        | { message_id?: string; message?: string }
        | undefined;
      if (payload?.message_id) {
        const id = payload.message_id;
        set((state) => {
          const current = state.watches[sessionId] ?? empty;
          const messages = current.messages.map((entry) =>
            entry.id === id ? { ...entry, content: [] } : entry,
          );
          return {
            watches: {
              ...state.watches,
              [sessionId]: { ...current, messages, waiting: payload.message || "正在等待恢复" },
            },
          };
        });
        return;
      }
      patch(sessionId, { waiting: payload?.message || "正在等待恢复" });
      return;
    }

    case "session": {
      // The conversation's state rather than something said in it: a task menu is
      // wrong until it is told, and a title is not known until the first thing said
      // in the conversation. Carries the same position space as everything else, so a
      // window that reconnects is given the state it missed.
      const info = (event as { session?: SessionInfo }).session;
      if (!info?.id) return;
      useWork.getState().upsertSession(info);
      patch(sessionId, { running: info.state === "running" });
      return;
    }

    case "state_change": {
      // Progress is the proof the wait ended, whatever kind of progress.
      patch(sessionId, { waiting: "" });
      const message = (event.data as { message?: ChatMessage } | undefined)?.message;
      if (!message?.id) return;
      set((state) => {
        const current = state.watches[sessionId] ?? empty;
        const index = current.messages.findIndex((entry) => entry.id === message.id);
        const messages =
          index < 0
            ? // A tool's own message is not a turn of its own: its results belong to
              // the call that asked for them, in the assistant message above. Kept
              // separate, the step would show its input for ever and its result would
              // appear as a strange second bubble saying nothing.
              foldToolMessages([...current.messages, message])
            : current.messages.map((entry, at) => (at === index ? message : entry));
        return {
          watches: {
            ...state.watches,
            [sessionId]: {
              ...current,
              messages,
              streamingId: message.role === "assistant" ? message.id : current.streamingId,
            },
          },
        };
      });
      return;
    }

    case "partial": {
      // The retry has begun to answer. Progress is the proof the wait ended.
      patch(sessionId, { waiting: "" });
      const payload = event.data as
        | {
            message_id?: string;
            chunk?: { type?: string; text?: string; thinking?: string };
          }
        | undefined;
      const id = payload?.message_id;
      const chunk = payload?.chunk;
      if (!id || !chunk) return;
      const block = chunkToBlock(chunk);
      if (!block) return;
      set((state) => {
        const current = state.watches[sessionId] ?? empty;
        const index = current.messages.findIndex((entry) => entry.id === id);
        // The message is made here if it is not already on screen, because the core
        // publishes the whole assistant message only once the stream is over. Waiting
        // for that is waiting for the whole answer: the pieces would have nowhere to
        // go and would be thrown away, and the person who asked a question would sit
        // looking at an unchanged window until the last token arrived. The message
        // that is being written is therefore opened by its first piece and filled in
        // as the rest arrive, and the whole message replaces it when the turn ends.
        const messages =
          index < 0
            ? [
                ...current.messages,
                {
                  id,
                  role: "assistant" as const,
                  content: [block],
                  created_at: event.timestamp,
                },
              ]
            : current.messages.map((entry, at) =>
                at === index ? { ...entry, content: appendChunk(entry.content, block) } : entry,
              );
        return {
          watches: {
            ...state.watches,
            [sessionId]: { ...current, messages, streamingId: id, running: true },
          },
        };
      });
      return;
    }

    case "stream": {
      // One round of the model is over, and the turn is not: what follows is the
      // tools the model asked for, and then the model again. Ending the turn here
      // would spend the whole tool execution saying the work was done — the chain
      // relabelled, the shimmer gone, the stop button withdrawn — and then claim it
      // had started over when the next round's first piece landed.
      return;
    }
    case "done": {
      patch(sessionId, { running: false, streamingId: null, waiting: "" });
      return;
    }

    case "tool_call": {
      // Progress is the proof the wait ended.
      patch(sessionId, { waiting: "" });
      // The calls are shown the moment they exist, not when their answers come back:
      // a step that appears only finished was invisible for the whole time it worked,
      // which is the time a person is watching. The model's own message carries them
      // too, but that arrives as a whole-message replacement; this is the earlier and
      // the more precise word, and the input is on the page before the tool is run.
      const payload = event.data as
        | { message_id?: string; calls?: ContentBlock[] }
        | undefined;
      const id = payload?.message_id;
      const calls = (payload?.calls ?? []).filter((call) => call.tool_call_id);
      if (!id || calls.length === 0) return;
      const asBlock = (call: ContentBlock): ContentBlock => ({
        type: "tool_use",
        tool_call_id: call.tool_call_id,
        tool_name: call.tool_name,
        input: call.input,
      });
      set((state) => {
        const current = state.watches[sessionId] ?? empty;
        const index = current.messages.findIndex((entry) => entry.id === id);
        const incoming = calls.map(asBlock).filter(
          (call) =>
            !(index >= 0 &&
              current.messages[index]?.content.some(
                (piece) => piece.tool_call_id === call.tool_call_id,
              )),
        );
        if (incoming.length === 0) return state;
        const messages =
          index < 0
            ? [
                ...current.messages,
                {
                  id,
                  role: "assistant" as const,
                  content: incoming,
                  created_at: event.timestamp,
                },
              ]
            : current.messages.map((entry, at) =>
                at === index ? { ...entry, content: [...entry.content, ...incoming] } : entry,
              );
        return {
          watches: {
            ...state.watches,
            [sessionId]: { ...current, messages, streamingId: id, running: true },
          },
        };
      });
      return;
    }

    case "tool_result": {
      // The answer goes into the call that asked for it, the moment it settles: the
      // step's shimmer stops and its output shows without waiting for the rest of the
      // turn, which may still hold rounds of thinking and further calls. The result
      // also arrives later inside the tool's own message, so nothing is lost if this
      // frame is missed; this is the copy that arrives first.
      const payload = event.data as
        | {
            results?: {
              tool_call_id?: string;
              output?: unknown;
              is_error?: boolean;
              error?: string;
            }[];
          }
        | undefined;
      const results = (payload?.results ?? []).filter((one) => one.tool_call_id);
      if (results.length === 0) return;
      set((state) => {
        const current = state.watches[sessionId] ?? empty;
        const messages = current.messages.map((entry) =>
          entry.content.some((block) =>
            results.some((one) => one.tool_call_id === block.tool_call_id),
          )
            ? {
                ...entry,
                content: entry.content.map((block) => {
                  const match = results.find((one) => one.tool_call_id === block.tool_call_id);
                  if (!match || block.type !== "tool_use") return block;
                  return {
                    ...block,
                    output: match.output ?? (match.is_error ? match.error : undefined),
                    is_error: match.is_error,
                  };
                }),
              }
            : entry,
        );
        return { watches: { ...state.watches, [sessionId]: { ...current, messages } } };
      });
      return;
    }

    case "error": {
      const message = (event.data as { message?: string } | undefined)?.message;
      patch(sessionId, {
        running: false,
        streamingId: null,
        waiting: "",
        error: event.error || message || "这一轮出错了",
      });
      return;
    }

    case "reset": {
      // The stream has said it cannot account for what came between two positions, so
      // what is on screen may be missing something. The conversation is read again
      // rather than left looking whole.
      patch(sessionId, { loaded: false });
      void useConversation.getState().load(sessionId);
      return;
    }

    default:
      // A control notice, or something this build does not know. Neither belongs
      // between two messages.
      return;
  }
}

/**
 * One provider piece, in the block the message is stored as.
 *
 * The end of a stream is announced as a piece too, and it is a piece with nothing in
 * it: kept as a block it would be an empty paragraph at the end of the answer, and a
 * message with an empty block in it is a thing every later reader has to know to skip.
 */
function chunkToBlock(chunk: { type?: string; text?: string; thinking?: string }): ContentBlock | null {
  if (chunk.type === "thinking") {
    return chunk.thinking ? { type: "thinking", thinking: chunk.thinking } : null;
  }
  if (chunk.type === "text") {
    return chunk.text ? { type: "text", text: chunk.text } : null;
  }
  return null;
}

/**
 * A streaming piece is appended onto the trailing block of its own kind, not added
 * beside it.
 *
 * The model's stream arrives in pieces of a few characters, and a piece that became its
 * own block is drawn as its own paragraph: a line of two or three characters each, the
 * whole answer stretched into a narrow column until the turn ends and the whole message
 * replaces the pieces. Consecutive pieces of one kind are one paragraph in the making,
 * and only a change of kind — the thinking ending, the answer beginning — starts a new
 * block, because that is a boundary the reader sees.
 */
function appendChunk(content: ContentBlock[], block: ContentBlock): ContentBlock[] {
  const last = content[content.length - 1];
  if (!last || last.type !== block.type) return [...content, block];
  if (block.type === "text") {
    return [...content.slice(0, -1), { ...last, text: (last.text ?? "") + (block.text ?? "") }];
  }
  if (block.type === "thinking") {
    return [
      ...content.slice(0, -1),
      { ...last, thinking: (last.thinking ?? "") + (block.thinking ?? "") },
    ];
  }
  // A tool call arrives whole or not at all: pieces of one are not text to be joined.
  return [...content, block];
}

/**
 * A tool's own message is folded into the call that asked for it.
 *
 * The core records a tool's answer as a message of its own, the way the model will
 * read it — but a turn on the page is one speaker's, and a bubble holding nothing but
 * an answer, addressed to nobody, is a bubble that reads like a second voice. Its
 * results are carried up into the assistant message's matching calls and the empty
 * shell is dropped. The same walk serves the live stream and the history read, so a
 * window opened afterwards reads the shape the one that watched was given.
 */
function foldToolMessages(messages: ChatMessage[]): ChatMessage[] {
  const kept: ChatMessage[] = [];
  for (const message of messages) {
    const results =
      message.role === "tool"
        ? message.content.filter((block) => block.type === "tool_result")
        : [];
    const target =
      results.length > 0 && results.length === message.content.length
        ? [...kept].reverse().find((one) => one.role === "assistant")
        : undefined;
    if (!target) {
      kept.push(message);
      continue;
    }
    kept[kept.indexOf(target)] = {
      ...target,
      content: target.content.map((block) => {
        if (block.type !== "tool_use") return block;
        const match = results.find((one) => one.tool_call_id === block.tool_call_id);
        return match
          ? {
              ...block,
              output: match.output,
              is_error: match.is_error || block.is_error,
            }
          : block;
      }),
    };
  }
  return kept;
}
