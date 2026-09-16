import { Component, computed, effect, inject, input, signal, untracked } from '@angular/core';
import { Api } from '../api';
import type { DryRunPage, Plan, PlanEntry } from '../models';
import * as fmt from '../format';

const allOps = ['add', 'replace', 'delete'];

const pageSize = 200;

/** DryRunList pages through the complete list of a pair's latest dry run. The
 *  daemon keeps one list per pair, so a card for an older run of the same pair
 *  says it has been replaced rather than showing the newer list under it. */
@Component({
  selector: 'app-dry-run-list',
  imports: [],
  templateUrl: './dry-run-list.html',
  styleUrl: './dry-run-list.css',
})
export class DryRunList {
  private readonly api = inject(Api);
  readonly fmt = fmt;
  readonly allOps = allOps;

  readonly pairId = input.required<number>();
  readonly startedAt = input.required<string>();
  readonly plan = input.required<Plan>();
  readonly open = input(false);

  readonly shown = signal<boolean | undefined>(undefined);
  readonly visible = computed(() => this.shown() ?? this.open());

  readonly ops = signal<string[]>([...allOps]);
  readonly query = signal('');
  readonly offset = signal(0);

  readonly page = signal<DryRunPage | undefined>(undefined);
  readonly loading = signal(false);
  readonly error = signal('');

  /** Answers to a filter that has since changed are dropped rather than raced. */
  private seq = 0;

  readonly superseded = computed(() => {
    const p = this.page();
    return !!p && Date.parse(p.startedAt) !== Date.parse(this.startedAt());
  });

  readonly total = computed(() => {
    const p = this.plan();
    return p.add + p.replace + p.vanished;
  });

  constructor() {
    effect(() => {
      if (!this.visible()) return;
      const filter = { pair: this.pairId(), ops: this.ops(), q: this.query(), offset: this.offset() };
      untracked(() => void this.load(filter));
    });
  }

  has(op: string): boolean {
    return this.ops().includes(op);
  }

  toggleOp(op: string, on: boolean): void {
    this.offset.set(0);
    this.ops.update((ops) =>
      on ? allOps.filter((o) => o === op || ops.includes(o)) : ops.filter((o) => o !== op),
    );
  }

  search(q: string): void {
    this.offset.set(0);
    this.query.set(q.trim());
  }

  step(direction: 1 | -1): void {
    this.offset.update((o) => Math.max(0, o + direction * pageSize));
  }

  last(p: DryRunPage): number {
    return Math.min(p.total, p.offset + p.entries.length);
  }

  /** What a sync would actually do with the entry, which for a vanished file is
   *  the pair's mode and guard rather than the op alone. */
  fate(e: PlanEntry, plan: Plan): { label: string; cls: string } {
    if (plan.database && e.path === plan.database.path) {
      return { label: `${e.op} · lock gate`, cls: '' };
    }
    switch (e.op) {
      case 'add':
        return { label: 'download', cls: 'ok' };
      case 'replace':
        return { label: 'replace', cls: '' };
      case 'delete':
        if (plan.mode === 'additive') return { label: 'vanished · kept', cls: '' };
        if (plan.guard && !plan.approved) return { label: 'delete · on hold', cls: 'warn' };
        return { label: 'delete', cls: 'bad' };
      default:
        return { label: e.op, cls: '' };
    }
  }

  private async load(filter: { pair: number; ops: string[]; q: string; offset: number }): Promise<void> {
    const seq = ++this.seq;

    // The daemon reads an empty op list as "no filter".
    if (filter.ops.length === 0) {
      this.page.update((p) => p && { ...p, total: 0, offset: 0, entries: [] });
      return;
    }

    this.loading.set(true);
    try {
      const page = await this.api.dryRun(filter.pair, {
        op: filter.ops.length === allOps.length ? undefined : filter.ops,
        q: filter.q,
        offset: filter.offset,
        limit: pageSize,
      });
      if (seq !== this.seq) return;
      this.page.set(page);
      this.error.set('');
    } catch (err) {
      if (seq === this.seq) this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      if (seq === this.seq) this.loading.set(false);
    }
  }
}
