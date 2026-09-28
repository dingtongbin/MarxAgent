import { useEffect, useState } from "react";
import { Brain, ChevronDown, LoaderCircle, MessageSquare, Wrench } from "lucide-react";
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from "@/components/ai-elements/conversation";
import {
  Message,
  MessageContent,
  MessageResponse,
} from "@/components/ai-elements/message";
import {
  Reasoning,
  ReasoningContent,
  ReasoningTrigger,
} from "@/components/ai-elements/reasoning";
import { Tool, ToolInput, ToolOutput } from "@/components/ai-elements/tool";
import { Shimmer } from "@/components/ai-elements/shimmer";
import { CollapsibleContent } from "@/components/ui/collapsible";
import { Composer } from "./Composer";
import {
  WorkspacePanel,
  panelTabs,
  type PanelPage,
  type PanelTab,
} from "./WorkspacePanel";
import { useConversation } from "@/store/conversation";
import { useWork } from "@/store/work";
import { messageText, type ChatMessage, type ContentBlock } from "@/types";
import d from "./NewSessionPanel.module.css";
import w from "./WorkspacePanel.module.css";

/**
 * A conversation.
 *
 * The log fills the window and the composer sits at the foot of it.
 *
 * The composer wears the launcher's border but none of its surface. A raised panel in
 * the middle of a reading column would be the one decoration the place a person reads
 * most cannot afford — so the card is here for its edge and its padding, and the
 * override in the stylesheet is what takes the surface off it.
 */
export function SessionView({ sessionId }: { sessionId: string }) {
  const watch = useConversation((state) => state.watches[sessionId]);
  const load = useConversation((state) => state.load);
  const send = useConversation((state) => state.send);
  const interrupt = useConversation((state) => state.interrupt);
  const session = useWork((state) =>
    state.sessions.find((entry) => entry.id === sessionId),
  );
  const windowModel = useWork((state) => state.model);
  const setModel = useWork((state) => state.setModel);
  // The model this conversation is on, which is its own rather than the window's: a
  // conversation that has been talking to one model says so, and keeps saying so after
  // the window has moved on to another. The same value is what the turn is sent with,
  // because a chip naming one model while the words go to another is the one lie this
  // control must not tell.
  const model = session?.model || windowModel;

  // The level the next turn runs at. It starts at what the conversation was
  // last sent under, and switching it here changes only what the composer
  // says: the turn sent next is the turn that runs under the level shown,
  // which is when the switch becomes real and is recorded with the
  // conversation.
  const [execution, setExecution] = useState(session?.execution ?? "build");
  // A conversation that says otherwise — its own record of where it is — is
  // what the switch follows once it is known, until it is changed again.
  useEffect(() => {
    setExecution(session?.execution ?? "build");
  }, [session?.execution]);

  // The workspace's pages, and the one showing. Each tool opens a page of its
  // kind — an existing one is brought forward rather than opened twice — and
  // every page can be closed. A panel with no pages left is a panel closed.
  const [panelPages, setPanelPages] = useState<PanelPage[]>([]);
  const [panelActive, setPanelActive] = useState<string | null>(null);

  const openPanelPage = (kind: PanelTab) => {
    const existing = panelPages.find((page) => page.kind === kind);
    if (existing) {
      setPanelActive(existing.id);
      return;
    }
    const id = `${kind}-${panelPages.length + 1}`;
    setPanelPages([...panelPages, { id, kind }]);
    setPanelActive(id);
  };

  // A tab is mounted once, and a conversation goes on happening whether it is in
  // front of the person or not, so the history and the stream are asked for when it
  // is shown rather than on every render.
  useEffect(() => {
    void load(sessionId);
  }, [load, sessionId]);

  const messages = watch?.messages ?? [];
  const running = watch?.running ?? session?.state === "running";
  // The wait the last round is pausing through, if one is. It shows inside
  // that round's chain, as part of the chain rather than as a notice beside
  // it, because the turn has not stopped and the chain is where it is being
  // drawn.
  const waiting = watch?.waiting ?? "";

  // A tab can point at a conversation that is gone. It is said as what it is, with
  // the two things a person can do about it, because a tab that can never load is
  // otherwise a tab that has to be guessed at.
  if (watch?.gone) {
    return (
      <div className="flex min-h-0 min-w-0 flex-1 items-center justify-center">
        <div className={d.gone}>
          <MessageSquare size={24} strokeWidth={1.5} />
          <p className={d.goneTitle}>这个对话已经不在了</p>
          <p className={d.goneBody}>
            它可能是在另一个数据目录里创建的，或者已经被清掉了。这个标签指向的东西没有了。
          </p>
          <div className={d.goneActions}>
            <button
              className={d.goneButton}
              onClick={() => useWork.getState().openFixed("new")}
            >
              新建会话
            </button>
            <button
              className={`${d.goneButton} ${d.goneButtonQuiet}`}
              onClick={() => useWork.getState().closeTab(sessionId)}
            >
              关闭这个标签
            </button>
          </div>
        </div>
      </div>
    );
  }

  // One round per message the user sent: everything the model did in answer
  // to it — the thinking, every tool call and its results, the words in
  // between — is one chain, whatever length the chain is.
  const rounds = roundsOf(messages);
  const conversation = (
    <>
      <Conversation className={d.conversation}>
        <ConversationContent className={d.conversationBody}>
          {messages.length === 0 && !running ? (
            <ConversationEmptyState
              icon={<MessageSquare size={26} strokeWidth={1.5} />}
              title="还没有对话"
              description="在下面写下第一条消息。"
            />
          ) : (
            rounds.map((round, index) => (
              <RoundView
                key={round.user.id}
                round={round}
                live={running && index === rounds.length - 1}
                waiting={waiting}
              />
            ))
          )}
          {/* The turn is on screen from the moment it is asked for, before there is
              anything in it to show. Waiting for the first token to put a row here
              would leave the window unchanged for the length of a model's first
              thought, which is the whole of the wait a person spends wondering
              whether the button worked. */}
          {messages.length === 0 && running ? (
            <ProcessChain steps={[]} streaming waiting={waiting} />
          ) : null}
        </ConversationContent>
        <ConversationScrollButton />
      </Conversation>

      {watch?.error ? <div className={d.conversationError}>{watch.error}</div> : null}

      <div className="shrink-0 px-6 pb-5">
        <div className="mx-auto w-[760px]">
          {/* The same one element that gathers the composer's parts into a single
              bordered thing, for the same reason: the Composer returns a fragment. */}
          <div className={d.card}>
            <Composer
              variant="session"
              // The conversation is the mode it was opened in. A mode that arrived
              // from a build before there were modes is a coding conversation: that
              // is the name its storage is spelled in.
              mode={session?.mode === "chat" ? "chat" : "coding"}
              workdir={session?.project ?? ""}
              model={model}
              onModelChange={setModel}
              execution={execution}
              onExecutionChange={setExecution}
              onSubmit={({ text, execution: level }) =>
                void send(sessionId, text, model || undefined, level)
              }
              running={running}
              onInterrupt={() => void interrupt(sessionId)}
            />
          </div>
        </div>
      </div>
    </>
  );

  // A chat conversation has no workspace and no navigation: it is a column of
  // words and its composer, as it has always been. A coding one is a shell:
  // a navigation bar over a split, the conversation on the left and the
  // workspace panel on the right, the panel opened by a tool in the bar.
  const coding = session?.mode !== "chat";
  if (!coding) {
    return <div className="flex min-h-0 min-w-0 flex-1 flex-col">{conversation}</div>;
  }
  return (
    <div className={w.shell}>
      {/* The navigation bar. Its tools sit on the right, in the order they were
          declared — the panel's own tab order — and never drawn backwards. */}
      <nav className={w.nav}>
        <div className={w.navTools}>
          {panelTabs.map((entry) => {
            const Icon = entry.icon;
            const on = panelPages.some((page) => page.kind === entry.value);
            return (
              <button
                key={entry.value}
                className={`${w.navBtn} ${on ? w.navBtnOn : ""}`}
                onClick={() => openPanelPage(entry.value)}
                aria-pressed={on}
                title={entry.label}
              >
                {Icon ? <Icon size={15} strokeWidth={2} /> : <FolderGlyph />}
              </button>
            );
          })}
        </div>
      </nav>
      <div className={w.split}>
        <div className={w.main}>{conversation}</div>
        {panelPages.length > 0 ? (
          <WorkspacePanel
            project={session?.project ?? ""}
            pages={panelPages}
            activeId={panelActive}
            onActivate={setPanelActive}
            onClosePage={(id) => {
              const kept = panelPages.filter((page) => page.id !== id);
              setPanelPages(kept);
              if (panelActive === id) setPanelActive(kept[kept.length - 1]?.id ?? null);
            }}
          />
        ) : null}
      </div>
    </div>
  );
}

/** The mark the files tool carries, which the tool set itself does not name. */
function FolderGlyph() {
  return (
    <svg width={15} height={15} viewBox="0 0 24 24" fill="none" stroke="currentColor"
      strokeWidth={2} strokeLinecap="round" strokeLinejoin="round">
      <path d="M4 5h5l2 2h9v11a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V4z" />
    </svg>
  );
}

type Round = {
  user: ChatMessage;
  rest: ChatMessage[];
};

/**
 * One round per message the user sent: everything the model did in answer to
 * it, kept in the order it happened.
 */
function roundsOf(messages: ChatMessage[]): Round[] {
  const rounds: Round[] = [];
  for (const message of messages) {
    if (message.role === "user") {
      rounds.push({ user: message, rest: [] });
    } else if (rounds.length > 0) {
      rounds[rounds.length - 1].rest.push(message);
    }
  }
  return rounds;
}

/**
 * One action in the chain, named after what it is: a thought, a call, or a
 * sentence the model said on the way to its answer.
 */
type Step = {
  key: string;
  name: string;
  kind: "thinking" | "tool" | "text";
  text: string;
  call?: ContentBlock;
  result?: ContentBlock;
};

/**
 * One round, drawn as what it is: one chain of everything the model did, and
 * the summary it ended with outside that chain.
 *
 * The chain carries the thinking, every call and its result, and the words
 * said between actions, in the order they happened — a round that called tools
 * four times is four calls in one chain, not four chains. The message the
 * round ended on is the only one whose words go outside: it is the summary the
 * round exists to produce, and it is not folded. A round that was interrupted
 * says so where the summary would be, because that is what it produced.
 */
function RoundView({
  round,
  live,
  waiting,
}: {
  round: Round;
  /** The round the turn is still answering, if it is. */
  live: boolean;
  /** The pause the turn is waiting through, if one is. */
  waiting: string;
}) {
  // The message the round ended on: the one whose words are the summary.
  let final: ChatMessage | undefined;
  for (const message of round.rest) {
    if (message.role === "assistant") final = message;
  }
  const { steps, body } = roundSteps(round.rest, final);
  const streaming = live;
  // A chain is drawn when there is something in it, or while the round is
  // still going — an empty chain that is still running is the wait for the
  // model's first word, and an empty chain that has stopped was never there.
  const hasChain = steps.length > 0 || (live && (waiting !== "" || body.length === 0));

  return (
    <>
      <Message from="user">
        <MessageContent>
          <MessageResponse>{messageText(round.user)}</MessageResponse>
        </MessageContent>
      </Message>
      <Message from="assistant">
        <MessageContent>
          {hasChain ? <ProcessChain steps={steps} streaming={streaming} waiting={waiting} /> : null}
          {body.map((block, index) => (
            <Block key={`${block.type}-${index}`} block={block} index={index} />
          ))}
          {final?.incomplete ? (
            <div className={d.incomplete}>这一轮被中断了，上面是已经说出来的部分。</div>
          ) : null}
        </MessageContent>
      </Message>
    </>
  );
}

/**
 * The steps of one round, and the words that belong outside the chain.
 *
 * Every message the model sent in the round contributes its thinking and its
 * calls to the chain, in the order they happened; a result folds into the
 * call that asked for it, because a call and what came back are one action.
 * The words of the round's final message are the body, and the words of
 * every earlier message are steps too — a sentence the model said before
 * calling another tool is something it did, not the summary.
 */
function roundSteps(
  messages: ChatMessage[],
  final: ChatMessage | undefined,
): { steps: Step[]; body: ContentBlock[] } {
  const steps: Step[] = [];
  const body: ContentBlock[] = [];
  // Where a tool result belongs: the step that asked for it.
  const byCall = new Map<string, Step>();
  let index = 0;

  for (const message of messages) {
    for (const block of message.content) {
      index++;
      if (block.type === "thinking") {
        if (!block.thinking) continue;
        steps.push({
          key: `think-${index}`,
          name: "思考",
          kind: "thinking",
          text: block.thinking,
        });
        continue;
      }
      if (block.type === "tool_use") {
        const step: Step = {
          key: `call-${index}`,
          name: block.tool_name || "调用工具",
          kind: "tool",
          text: "",
          call: block,
        };
        steps.push(step);
        if (block.tool_call_id) byCall.set(block.tool_call_id, step);
        continue;
      }
      if (block.type === "tool_result") {
        const step = block.tool_call_id ? byCall.get(block.tool_call_id) : undefined;
        if (step) {
          step.result = block;
          continue;
        }
        steps.push({
          key: `result-${index}`,
          name: block.tool_name || "工具结果",
          kind: "tool",
          text: "",
          result: block,
        });
        continue;
      }
      // The round's own final words are not steps. Everything else the model
      // said — a sentence between two calls — is a step of the chain.
      if (message.id === final?.id) {
        body.push(block);
        continue;
      }
      steps.push({
        key: `text-${index}`,
        name: "说明",
        kind: "text",
        text: block.text ?? "",
      });
    }
  }
  return { steps, body };
}

/**
 * The reasoning, folded away, with each action folded away on its own.
 *
 * Two levels because there are two things a person wants separately: whether to look
 * at what was reasoned at all, and how much of it. A single long body behind one
 * toggle makes the second question unanswerable.
 *
 * The row is titled by what is happening rather than by what the thing is called, so
 * a person glancing at a conversation can tell a turn that is still being reasoned
 * from one that has finished without opening anything. The mark beside it belongs to
 * the reasoning and not to the answer, so it is there while the reasoning is happening
 * and gone once it has stopped: a spinner left turning on a finished row would be
 * claiming the work is still going, and a tick left sitting there would be claiming
 * something about the answer that this row knows nothing about.
 *
 * The chain starts folded and stays as it is left. A fold that opened itself while a
 * model was working and shut itself a second later would move the page under whoever
 * was reading it, and somebody who opened it would watch it close without touching it.
 */
function ProcessChain({
  steps,
  streaming,
  waiting,
}: {
  steps: Step[];
  streaming: boolean;
  /** The pause the turn is waiting through, if one is. */
  waiting?: string;
}) {
  return (
    <Reasoning defaultOpen={false} isStreaming={streaming} className={d.chain}>
      <ReasoningTrigger className={d.chainTrigger}>
        {streaming ? (
          <LoaderCircle size={15} strokeWidth={2} className={`${d.chainIcon} ${d.chainIconSpin}`} />
        ) : null}
        <span className={d.chainLabel}>{streaming ? "推理中" : "推理结束"}</span>
        <span className={d.chainCount}>{steps.length} 步</span>
        <ChevronDown size={15} strokeWidth={2} className={d.chainChev} />
      </ReasoningTrigger>
      {/* Not ReasoningContent: that one renders a string of markdown, and what is in
          here is a list of things rather than text. The chain above is still the
          ai-elements component doing the folding; this is only the box it folds. */}
      <CollapsibleContent className={d.chainBody}>
        {steps.map((step, index) =>
          // The words the model said between actions are part of the chain but not
          // part of the folding: they are prose, drawn bare, in the order they were
          // said, without a fold of their own and without an icon.
          step.kind === "text" ? (
            <MessageResponse key={step.key}>{step.text}</MessageResponse>
          ) : (
            <Step
              key={step.key}
              step={step}
              live={
                streaming &&
                index === steps.length - 1 &&
                // A finished step does not shine: its result has landed, and a row that
                // claims to be working while its output is already on the page says the
                // opposite of what it shows. The last step of the turn is live only while
                // it has nothing to show for itself yet.
                !(step.kind === "tool" && step.result)
              }
            />
          ),
        )}
        {/* The wait is part of the chain, as the last row: the turn has not stopped,
            and a pause it is waiting through is something it is doing. */}
        {streaming && waiting ? (
          <div className={d.chainWait}>
            <LoaderCircle size={13} strokeWidth={2} className={d.chainIconSpin} />
            <span>{waiting}</span>
          </div>
        ) : null}
      </CollapsibleContent>
    </Reasoning>
  );
}

/**
 * One action, folded away on its own and capped at ten lines.
 *
 * Every row is marked with the kind of thing it is: a thought is a thought and a call
 * is a call, and a list where those are indistinguishable is a list a person has to
 * open each row of to find out what the model was doing.
 *
 * The row being written shines. It is the one step of the turn that is still moving,
 * and without that the only sign anything is happening is the character count ticking
 * over in a folded row nobody is looking at.
 */
function Step({ step, live }: { step: Step; live: boolean }) {
  const icon =
    step.kind === "tool" ? (
      <Wrench size={14} strokeWidth={2} className={d.chainStepIcon} />
    ) : (
      <Brain size={14} strokeWidth={2} className={d.chainStepIcon} />
    );

  return (
    <Reasoning defaultOpen={false} className={d.chainStep}>
      <ReasoningTrigger className={d.chainStepTrigger}>
        {icon}
        {live ? (
          <Shimmer as="span" duration={1.4} className={d.chainStepName}>
            {step.name}
          </Shimmer>
        ) : (
          <span className={d.chainStepName}>{step.name}</span>
        )}
        {step.kind === "thinking" ? (
          <span className={d.chainStepSize}>{step.text.length} 字</span>
        ) : null}
        <ChevronDown size={14} strokeWidth={2} className={d.chainStepChev} />
      </ReasoningTrigger>
      {step.kind === "tool" ? (
        <CollapsibleContent className={d.chainStepBody}>
          <Tool>
            {step.call ? <ToolInput input={step.call.input} /> : null}
            {step.result ? (
              <ToolOutput
                output={step.result.output}
                errorText={step.result.is_error ? "这一步失败了" : undefined}
              />
            ) : null}
          </Tool>
        </CollapsibleContent>
      ) : (
        <ReasoningContent className={d.chainStepBody}>{step.text}</ReasoningContent>
      )}
    </Reasoning>
  );
}

/** What was said, as opposed to what was done. */
function Block({ block, index }: { block: ContentBlock; index: number }) {
  if (block.type === "text") {
    return <MessageResponse key={index}>{block.text ?? ""}</MessageResponse>;
  }
  // A block this build does not know how to draw is shown as itself rather than
  // dropped: a message with a piece missing and no sign of it is worse than an ugly
  // one, because nothing about it looks wrong.
  return (
    <pre key={index} className={d.unknownBlock}>
      {JSON.stringify(block, null, 2)}
    </pre>
  );
}
