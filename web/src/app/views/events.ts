import { Component, computed, effect, inject, signal } from '@angular/core';
import { Api } from '../api';
import { Live } from '../live';
import type { EventRow, Level, PairView } from '../models';
import * as fmt from '../format';

const allLevels: Level[] = ['debug', 'info', 'warn', 'error'];

/** The closed vocabulary of event kinds, from the Kind* constants in
 *  store/events.go, each with the label the filter shows. The raw kind is what
 *  the table prints; this list only exists so the select reads like English. */
const allKinds: { kind: string; label: string }[] = [
  { kind: 'startup', label: 'Started' },
  { kind: 'shutdown', label: 'Stopped' },
  { kind: 'config.changed', label: 'Config changed' },
  { kind: 'tunnel.up', label: 'Tunnel up' },
  { kind: 'tunnel.down', label: 'Tunnel down' },
  { kind: 'remote.probe', label: 'Remote probed' },
  { kind: 'pair.changed', label: 'Pair changed' },
  { kind: 'scan.started', label: 'Scan started' },
  { kind: 'scan.finished', label: 'Scan finished' },
  { kind: 'sync.started', label: 'Sync started' },
  { kind: 'sync.finished', label: 'Sync finished' },
  { kind: 'transfer.failed', label: 'Transfer failed' },
  { kind: 'adopt.finished', label: 'Adoption finished' },
  { kind: 'delete.finished', label: 'Deletion finished' },
  { kind: 'delete.blocked', label: 'Deletion guard tripped' },
  { kind: 'delete.decided', label: 'Deletion decided' },
  { kind: 'trash.pruned', label: 'Trash pruned' },
  { kind: 'trash.restored', label: 'Trash restored' },
  { kind: 'jcc.db.replaced', label: 'ClipCorn DB replaced' },
  { kind: 'jcc.db.skipped', label: 'ClipCorn DB skipped' },
  { kind: 'jcc.db.rollback', label: 'ClipCorn DB rolled back' },
  { kind: 'jcc.lock.stale', label: 'Source lock stale' },
  { kind: 'schedule.window', label: 'Schedule window' },
  { kind: 'space.low', label: 'Free space low' },
  { kind: 'error', label: 'Error' },
];

interface Filter {
  levels: Level[];
  kind: string;
  pair: number;
  limit: number;
}

/** EventsPage is the event log: scans, syncs, DB replacements and skips,
 *  restarts, guards and errors (DESIGN.md §4). */
@Component({
  selector: 'app-events',
  imports: [],
  templateUrl: './events.html',
  styleUrl: './events.css',
})
export class EventsPage {
  private readonly api = inject(Api);
  private readonly live = inject(Live);
  readonly fmt = fmt;
  readonly allLevels = allLevels;
  readonly allKinds = allKinds;

  readonly levels = signal<Level[]>(['info', 'warn', 'error']);
  readonly kind = signal('');
  readonly pair = signal(0);
  readonly limit = signal(200);

  readonly loading = signal(false);
  readonly error = signal('');
  readonly pairs = signal<PairView[]>([]);

  /** The ids whose data is unfolded. Held as a Set that is replaced rather than
   *  mutated: a signal given the same object back never fires. */
  readonly expanded = signal<ReadonlySet<number>>(new Set<number>());

  private readonly fetched = signal<EventRow[]>([]);
  private seq = 0;

  private readonly filter = computed<Filter>(() => ({
    levels: this.levels(),
    kind: this.kind(),
    pair: this.pair(),
    limit: this.limit(),
  }));

  private readonly names = computed(() => new Map(this.pairs().map((p) => [p.id, p.name])));

  readonly fresh = computed(() => {
    const filter = this.filter();
    const seen = new Set(this.fetched().map((r) => r.id));
    return this.live.events().filter((r) => !seen.has(r.id) && passes(r, filter));
  });

  readonly rows = computed(() => [...this.fresh(), ...this.fetched()]);

  constructor() {
    this.live.connect();
    void this.loadPairs();
    effect(() => void this.load(this.filter()));
  }

  has(level: Level): boolean {
    return this.levels().includes(level);
  }

  toggleLevel(level: Level, on: boolean): void {
    this.levels.update((levels) =>
      on
        ? allLevels.filter((l) => l === level || levels.includes(l))
        : levels.filter((l) => l !== level),
    );
  }

  levelClass(level: Level): string {
    switch (level) {
      case 'error':
        return 'bad';
      case 'warn':
        return 'warn';
      case 'debug':
        return 'dim';
      default:
        return '';
    }
  }

  nameOf(id: number | undefined): string {
    return (id && this.names().get(id)) || '—';
  }

  toggle(row: EventRow): void {
    if (!row.data) return;
    this.expanded.update((ids) => {
      const next = new Set(ids);
      if (!next.delete(row.id)) next.add(row.id);
      return next;
    });
  }

  json(data: Record<string, unknown>): string {
    return JSON.stringify(data, null, 2);
  }

  private async load(filter: Filter): Promise<void> {
    const seq = ++this.seq;

    // An empty level list reads as "no filter" at the other end, so no level
    // ticked has to mean no rows here.
    if (filter.levels.length === 0) {
      this.fetched.set([]);
      return;
    }

    this.loading.set(true);
    try {
      const rows = await this.api.events({
        level: filter.levels,
        kind: filter.kind ? [filter.kind] : undefined,
        pair: filter.pair || undefined,
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

function passes(row: EventRow, filter: Filter): boolean {
  if (filter.pair && row.pairId !== filter.pair) return false;
  if (filter.kind && row.kind !== filter.kind) return false;
  return filter.levels.includes(row.level);
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
