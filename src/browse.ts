import { z } from "zod";
import { resolve } from "node:path";
import type { MemoryStore } from "./store";
import { folderSelection, type ReadProfile } from "./retrieval";

export const browseSchema = z.strictObject({
  view: z.enum(["pages", "page", "history"]).default("pages"),
  id: z.string().min(1).optional(), folder: z.string().min(1).optional(),
  period: z.enum(["day", "week", "month", "year"]).default("month"),
  limit: z.number().int().min(1).max(100).default(40), offset: z.number().int().nonnegative().default(0),
  profile: z.string().min(1).optional(),
});
export const browseItemSchema = z.object({ id: z.string(), kind: z.enum(["page", "observation", "timeline"]),
  line: z.string().nullable(), parent: z.string().nullable().optional(), category: z.string().optional(),
  happened: z.string().nullable().optional() });
export const browseResultSchema = z.object({ items: z.array(browseItemSchema), more: z.boolean(),
  page: browseItemSchema.optional(), children: z.array(browseItemSchema).optional() });

/** Compact read projections. Detail and evidence remain explicit open operations. */
export function browseMemory(store: MemoryStore, args: z.output<typeof browseSchema>, profile: ReadProfile) {
  const { limit, offset } = args;
  if (args.view === "history") {
    const rows = store.db.query(`SELECT id,'timeline' kind,headline line,starts happened FROM timeline_nodes
      WHERE kind=? ORDER BY starts DESC LIMIT ? OFFSET ?`).all(args.period, limit + 1, offset);
    return { items: rows.slice(0, limit), more: rows.length > limit };
  }
  const allowed = new Set(store.descendantPageSlugs(profile.root));
  const visible = new Set((store.db.query(`WITH RECURSIVE visible(slug) AS (
    SELECT DISTINCT op.page_slug FROM observation_pages op JOIN observations o ON o.id=op.observation_id
      WHERE o.sensitivity<=? AND o.replaced_by IS NULL AND o.forgotten_at IS NULL
    UNION SELECT p.parent FROM pages p JOIN visible v ON v.slug=p.slug WHERE p.parent IS NOT NULL
  ) SELECT slug FROM visible`).all(profile.sensitivityMax) as { slug: string }[]).map(row => row.slug));
  const pages = store.readablePages(profile.sensitivityMax).filter(page => allowed.has(page.slug) && visible.has(page.slug))
    .map(page => ({ id: page.slug, kind: "page" as const, line: page.line, parent: page.parent, category: page.category }));
  if (args.view === "page") {
    const page = pages.find(page => page.id === args.id);
    if (!page) throw new Error(`Unknown or unavailable page ${args.id}`);
    const rows = store.db.query(`SELECT o.id,'observation' kind,o.line,o.happened FROM observations o
      JOIN observation_pages op ON op.observation_id=o.id WHERE op.page_slug=? AND o.sensitivity<=?
      AND o.replaced_by IS NULL AND o.forgotten_at IS NULL ORDER BY o.weight DESC,o.entered DESC,o.id LIMIT ? OFFSET ?`)
      .all(page.id, profile.sensitivityMax, limit + 1, offset);
    return { page, children: pages.filter(child => child.parent === page.id), items: rows.slice(0, limit), more: rows.length > limit };
  }
  const scores = args.folder ? folderSelection(store, resolve(args.folder), profile).pageScores : null;
  const selected = pages.filter(page => page.id !== profile.root && (!scores || scores.has(page.id)))
    .sort((a, b) => (scores ? (scores.get(b.id) ?? 0) - (scores.get(a.id) ?? 0) : 0) || a.id.localeCompare(b.id));
  return { items: selected.slice(offset, offset + limit), more: selected.length > offset + limit };
}
