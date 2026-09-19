import { commandCatalog, runCommand, type CommandName, type CommandResult } from "./commands";
import { loadConfig } from "./config";
import { EmbeddingClient } from "./embeddings";
import { drain, submitSource } from "./ingestion";
import { prepareDialogueSource, type DialogueSourceInput } from "./dialogue";
import { MemoryStore } from "./store";
import type { SourceInput } from "./types";

/** One in-process pd-memory runtime shared by Daimon and agent adapters. */
export class PdMemoryRuntime {
  private constructor(
    readonly store: MemoryStore,
    readonly config: Awaited<ReturnType<typeof loadConfig>>,
    readonly embeddings: EmbeddingClient,
  ) {}

  static async open(configPath: string): Promise<PdMemoryRuntime> {
    const config = await loadConfig(configPath);
    return new PdMemoryRuntime(new MemoryStore(config.workspace.db), config, new EmbeddingClient(config));
  }

  catalog() { return commandCatalog(); }

  command<N extends CommandName>(name: N, input: unknown): Promise<CommandResult<N>> {
    return runCommand({ store: this.store, config: this.config, embeddings: this.embeddings }, name, input);
  }

  async ingest(input: SourceInput | DialogueSourceInput, wait = false) {
    const submitted = await submitSource(this.store, "messages" in input ? prepareDialogueSource(input) : input);
    if (!wait || submitted.conflict) return submitted;
    this.store.recoverInterrupted(new Date(Date.now() - 30 * 60_000).toISOString());
    this.store.retryRecoverableFailures();
    const processed = await drain(this.store, this.config, [submitted.job]);
    const job = this.store.getJob(submitted.job);
    if (job?.status !== "completed") throw new Error(`pd-memory job ${submitted.job} ended ${job?.status ?? "unknown"}`);
    return { ...submitted, processed };
  }

  capturedSessionEntryIds(session: string): Set<string> {
    return this.store.capturedSessionEntryIds(session);
  }

  close(): void { this.store.close(); }
}
