import type { SourceInput } from "./types";
import { z } from "zod";

export const DIALOGUE_LIMITS = { userChars: 100_000, assistantChars: 5_000 } as const;
const TRUNCATION_MARKER = "\n...\n";

const dialogueMessageSchema = z.object({
  id: z.string().min(1).optional(), role: z.enum(["user", "assistant"]), text: z.string(),
  timestamp: z.string().refine((value) => Number.isFinite(Date.parse(value)), "Expected ISO time").optional(),
  speaker: z.string().trim().min(1).optional(),
});
export type DialogueMessage = z.infer<typeof dialogueMessageSchema>;

export interface PreparedDialogue {
  text: string;
  messages: Array<DialogueMessage & { originalChars: number; truncated: boolean }>;
  participants: string[];
  stats: { userChars: number; assistantChars: number; truncatedAssistantMessages: number };
}

export type DialogueSourceInput = Omit<SourceInput, "text" | "participants"> & { messages: DialogueMessage[] };

export function prepareDialogueSource(input: DialogueSourceInput): SourceInput {
  const { messages, ...source } = input;
  const dialogue = prepareDialogue(messages);
  return { ...source, text: dialogue.text, participants: dialogue.participants,
    metadata: { ...source.metadata, dialogue: dialogue.stats } };
}

/** Canonical compaction for every structured user/assistant source. */
export function prepareDialogue(input: unknown): PreparedDialogue {
  const messages = z.array(dialogueMessageSchema).parse(input);
  const prepared = messages.flatMap((message) => {
    const bounded = boundedDialogueText(message.role, message.text);
    return bounded === null ? [] : [{ ...message, ...bounded }];
  });
  if (!prepared.some((message) => message.role === "user")) throw new Error("Dialogue requires at least one user message");
  const participants = [...new Set(prepared.map((message) => message.speaker ?? (message.role === "user" ? "User" : "Assistant")))];
  return {
    text: prepared.map((message) => {
      const prefix = [message.timestamp, message.speaker ?? (message.role === "user" ? "User" : "Assistant")].filter(Boolean).join(" ");
      return `${prefix}:\n${message.text}`;
    }).join("\n\n"),
    messages: prepared,
    participants,
    stats: {
      userChars: prepared.filter((message) => message.role === "user").reduce((total, message) => total + message.text.length, 0),
      assistantChars: prepared.filter((message) => message.role === "assistant").reduce((total, message) => total + message.text.length, 0),
      truncatedAssistantMessages: prepared.filter((message) => message.role === "assistant" && message.truncated).length,
    },
  };
}

export function boundedDialogueText(role: "user" | "assistant", value: string): {
  text: string;
  originalChars: number;
  truncated: boolean;
} | null {
  const cleaned = value.replaceAll("\u0000", "").replaceAll("\r\n", "\n").replace(/\n{3,}/g, "\n\n").trim();
  if (cleaned.length === 0) return null;
  const limit = role === "user" ? DIALOGUE_LIMITS.userChars : DIALOGUE_LIMITS.assistantChars;
  if (cleaned.length <= limit) return { text: cleaned, originalChars: cleaned.length, truncated: false };
  const bodyLimit = limit - TRUNCATION_MARKER.length;
  const headChars = Math.max(1, Math.floor(bodyLimit / 3));
  const tailChars = Math.max(1, bodyLimit - headChars);
  return {
    text: `${cleaned.slice(0, headChars).trimEnd()}${TRUNCATION_MARKER}${cleaned.slice(-tailChars).trimStart()}`,
    originalChars: cleaned.length,
    truncated: true,
  };
}
