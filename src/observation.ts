import { z } from "zod";
import { AUTHORITIES, OBSERVATION_KINDS } from "./types";

export const nonEmptyText = z.string().trim().min(1);
export const score = z.number().min(0).max(1);
export const happened = z.string().regex(/^\d{4}-\d{2}-\d{2}(?:T.*Z)?$/)
  .refine((value) => Number.isFinite(Date.parse(value)), "Expected an ISO date or UTC datetime");

/** The persisted claim fields shared by extraction, integration, and direct commands. */
export const observationSchema = z.strictObject({
  line: nonEmptyText,
  body: nonEmptyText.nullable(),
  happened: happened.nullable(),
  claimant: nonEmptyText.nullable(),
  authority: z.enum(AUTHORITIES),
  kind: z.enum(OBSERVATION_KINDS),
  confidence: score,
  weight: score,
  durability: z.number().positive().nullable().describe("Relevance half-life in days, or null for permanent. Not a score."),
  sensitivity: score,
});

export type ObservationValue = z.infer<typeof observationSchema>;
