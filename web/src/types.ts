/**
 * What a conversation is made of, in the shape the server sends it.
 *
 * This is the same superset the core keeps: one content block list per message,
 * with the credentials that have to survive a round trip — a thinking signature,
 * an encrypted reasoning item — kept exactly as they arrived. None of it is
 * reshaped for the interface, because the interface is not where that reshaping
 * belongs: a block that lost its signature on the way to a component would fail
 * the next request to the model rather than merely look wrong here.
 *
 * The names match the server's JSON on purpose. A second set of names, translated
 * at the edges, is a second set of names to keep in step.
 */

/** One piece of a message. */
export type ContentBlock = {
  type: string;
  // text
  text?: string;
  // image
  image_data?: string;
  media_format?: string;
  image_source?: string;
  // tool_use
  tool_call_id?: string;
  tool_name?: string;
  input?: unknown;
  // tool_result
  output?: unknown;
  is_error?: boolean;
  // thinking, and the signature that has to go back with it
  thinking?: string;
  thinking_sig?: string;
  // reasoning, encrypted by the provider and opaque to us
  encrypted_content?: string;
};

/** One message. */
export type ChatMessage = {
  id: string;
  role: "system" | "user" | "assistant" | "tool";
  content: ContentBlock[];
  created_at?: string;
  metadata?: Record<string, unknown>;
  /**
   * The turn was cut off. It is kept rather than hidden, and shown as what it is:
   * half an answer is a record of what was said, and discarding it would be
   * discarding the record.
   */
  incomplete?: boolean;
};

/** One event on the stream. */
export type StreamEvent = {
  type: string;
  timestamp?: string;
  session_id?: string;
  agent_id?: string;
  error?: string;
  data?: unknown;
  // The conversation's own state, which is a separate path from what happened in
  // it: a task menu is wrong until it is told.
  session?: unknown;
  // A control notice: the stream is open, or it has to be reloaded.
  data_note?: string;
};

/** The text a message is made of, which is what a person reads. */
export function messageText(message: ChatMessage): string {
  let text = "";
  for (const block of message.content) {
    if (block.type === "text" && block.text) text += block.text;
  }
  return text;
}
