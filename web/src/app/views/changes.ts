import { Component, computed, effect, inject, signal } from '@angular/core';
import { Api } from '../api';
import { Live } from '../live';
import type { ChangeOp, ChangeRow, PairView } from '../models';
import * as fmt from '../format';

/** The four fates a file can meet, in the order the filter reads (store/files.go). */
const allOps: ChangeOp[] = ['add', 'replace', 'delete', 'fail'];

interface Filter {
  pair: number;
  ops: ChangeOp[];
  limit: number;
}

/** ChangesPage is the per-file history: what was added, replaced, deleted or
 *  failed, filterable by pair and op (DESIGN.md §4). The fetch is the history;
 *  the stream is what has happened since the page was opened, and the two are
 *  merged rather than kept apart. */
@Component({
  selector: 'app-changes',
  imports: [],
  templateUrl: './changes.html',
  styleUrl: './changes.css',
})
export class ChangesPage {
  private readonly api = inject(Api);
  private readonly live = inject(Live);
  readonly fmt = fmt;
  readonly allOps = allOps;

  readonly pair = signal(0);
  readonly ops = signal<ChangeOp[]>([...allOps]);
  readonly limit = signal(500);

  readonly loading = signal(false);
  readonly error = signal('');
  readonly pairs = signal<PairView[]>([]);

  private readonly fetched = signal<ChangeRow[]>([]);

  /** Answers to a filter that has since been changed are dropped rather than
   *  raced: a slow 2000-row read must not overwrite a fast 100-row one. */
  private seq = 0;

  private readonly filter = computed<Filter>(() => ({
    pair: this.pair(),
    ops: this.ops(),
    limit: this.limit(),
  }));

  private readonly names = computed(() => new Map(this.pairs().map((p) => [p.id, p.name])));

  /** What the stream has delivered that the fetch did not already carry, newest
   *  first. The limit bounds the fetch; live rows are newer than all of it and
   *  are added on top rather than counted against it. */
  readonly fresh = computed(() => {
    const filter = this.filter();
    const seen = new Set(this.fetched().map((r) => r.id));
    return this.live.changes().filter((r) => !seen.has(r.id) && passes(r, filter));
  });

  readonly rows = computed(() => [...this.fresh(), ...this.fetched()]);

  readonly totals = computed(() => {
    let added = 0;
    let replaced = 0;
    for (const row of this.rows()) {
      if (row.op === 'add') added += row.sizeAfter ?? 0;
      if (row.op === 'replace') replaced += row.sizeAfter ?? 0;
    }
    return { rows: this.rows().length, added, replaced };
  });

  constructor() {
    this.live.connect();
    void this.loadPairs();
    effect(() => void this.load(this.filter()));
  }

  has(op: ChangeOp): boolean {
    return this.ops().includes(op);
  }

  toggleOp(op: ChangeOp, on: boolean): void {
    this.ops.update((ops) =>
      on ? allOps.filter((o) => o === op || ops.includes(o)) : ops.filter((o) => o !== op),
    );
  }

  opClass(op: ChangeOp): string {
    switch (op) {
      case 'add':
        return 'ok';
      case 'delete':
        return 'warn';
      case 'fail':
        return 'bad';
      default:
        return '';
    }
  }

  /** A change outlives the pair it belonged to on purpose, so a row with no pair
   *  left is a fact to render rather than a lookup that failed. */
  nameOf(id: number | undefined): string {
    return (id && this.names().get(id)) || '—';
  }

  size(n: number | undefined): string {
    return n === undefined ? '-' : fmt.bytes(n);
  }

  private async load(filter: Filter): Promise<void> {
    const seq = ++this.seq;

    // No op ticked is a request for nothing. The daemon reads an empty op list as
    // "no filter", so asking it would answer with everything.
    if (filter.ops.length === 0) {
      this.fetched.set([]);
      return;
    }

    this.loading.set(true);
    try {
      const rows = await this.api.changes({
        pair: filter.pair || undefined,
        op: filter.ops,
        limit: filter.limit,
      });
      if (seq !== this.seq) return;
      this.fetched.set(rows);
      this.error.set('');
    } catch (err) {
      if (seq === this.seq) this.error.set(message(err));
    } finally {
      if (seq === this.seq) this.loading.set(false);
    }
  }

  private async loadPairs(): Promise<void> {
    try {
      this.pairs.set(await this.api.pairs());
    } catch (err) {
      this.error.set(message(err));
    }
  }
}

function passes(row: ChangeRow, filter: Filter): boolean {
  if (filter.pair && row.pairId !== filter.pair) return false;
  return filter.ops.includes(row.op);
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
