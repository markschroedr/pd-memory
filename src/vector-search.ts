export interface VectorMatch { id: string; score: number }
interface RankedVector extends VectorMatch { order: number }

/** Exact cosine search. Stream the corpus once for all query facets; retain only k results per facet. */
export function nearestVectors(
  rows: Iterable<{ id: string; vector: ArrayLike<number> }>, queries: ArrayLike<number>[], limit: number,
): VectorMatch[][] {
  if (!Number.isInteger(limit) || limit < 1) throw new Error("Vector result limit must be positive");
  const norms = queries.map(norm);
  const heaps = queries.map(() => new BestVectors(limit));
  let order = 0;
  for (const row of rows) {
    const length = norm(row.vector);
    queries.forEach((query, index) => {
      if (query.length !== row.vector.length) throw new Error("Embedding dimension mismatch");
      let dot = 0;
      for (let i = 0; i < query.length; i++) dot += query[i]! * row.vector[i]!;
      heaps[index]!.add({ id: row.id, score: dot / (norms[index]! * length), order });
    });
    order++;
  }
  return heaps.map((heap) => heap.sorted());
}

function norm(vector: ArrayLike<number>): number {
  let squared = 0;
  for (let i = 0; i < vector.length; i++) squared += vector[i]! ** 2;
  if (!Number.isFinite(squared) || squared === 0) throw new Error("Embedding must have a finite, non-zero norm");
  return Math.sqrt(squared);
}

// The worst retained result is the root. Earlier corpus order wins equal scores.
function worse(a: RankedVector, b: RankedVector): boolean {
  return a.score < b.score || (a.score === b.score && a.order > b.order);
}
class BestVectors {
  private items: RankedVector[] = [];
  constructor(private readonly limit: number) {}
  add(item: RankedVector): void {
    const items = this.items;
    if (items.length < this.limit) {
      items.push(item);
      let index = items.length - 1;
      while (index > 0) {
        const parent = Math.floor((index - 1) / 2);
        if (!worse(items[index]!, items[parent]!)) break;
        [items[index], items[parent]] = [items[parent]!, items[index]!];
        index = parent;
      }
      return;
    }
    if (!worse(items[0]!, item)) return;
    items[0] = item;
    let index = 0;
    while (index * 2 + 1 < items.length) {
      let child = index * 2 + 1;
      if (child + 1 < items.length && worse(items[child + 1]!, items[child]!)) child++;
      if (!worse(items[child]!, items[index]!)) break;
      [items[index], items[child]] = [items[child]!, items[index]!];
      index = child;
    }
  }
  sorted(): VectorMatch[] {
    return this.items.sort((a, b) => b.score - a.score || a.order - b.order).map(({ id, score }) => ({ id, score }));
  }
}
