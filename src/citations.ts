export interface CitationValidation { text: string; total: number; dropped: number }

export function citedIds(text: string): string[] {
  return [...new Set([...text.matchAll(/\[([^\[\]]*)\]/g)]
    .flatMap(match => match[1]!.split(",").map(id => id.trim()).filter(Boolean)))];
}

// Check evidence membership, not sentence boundaries. Broken brackets are removed.
export function validateCitations(input: string, allowed: Set<string>): CitationValidation {
  let total = 0, dropped = 0;
  const text = input.trim().replace(/\[([^\[\]]*)\]/g, (_group, contents: string) => {
    const ids = contents.split(",").map(id => id.trim()).filter(Boolean);
    total += ids.length;
    const valid = ids.filter(id => allowed.has(id));
    dropped += ids.length - valid.length;
    return valid.length ? `\u0001${valid.join(", ")}\u0002` : "";
  }).replace(/[\[\]]/g, "")
    .replace(/\u0001/g, "[").replace(/\u0002/g, "]").trim();
  if (total && dropped / total > 0.2) throw new Error(`Invalid citations: ${dropped}/${total} ids exceed 20%`);
  return { text, total, dropped };
}
