import { Component, computed, effect, inject, signal, untracked } from '@angular/core';
import { Api } from '../api';
import { Live } from '../live';
import type { DBBackup, PairView, Progress, Run } from '../models';
import { DryRunList } from './dry-run-list';
import * as fmt from '../format';

/** The job states in the order they are worth reading: what is left, what is
 *  moving, what is being checked, what went wrong, what landed. */
const queueStates = ['pending', 'running', 'verifying', 'failed', 'done'];

/** What each pair button does, shown as its tooltip and in the legend below the
 *  pairs. `jcc` marks the ones only a jcc pair has. */
const actions: { kind: string; label: string; help: string; jcc?: boolean }[] = [
  {
    kind: 'plan',
    label: 'Plan',
    help: 'Compares the last walk with what is here and counts what a sync would do, with a sample of the files. Asks the publisher nothing and changes nothing.',
  },
  {
    kind: 'dryrun',
    label: 'Dry run',
    help: 'Walks the publisher first, then lists every file a sync would download, replace or delete. Records the fresh walk and changes nothing else.',
  },
  {
    kind: 'scan',
    label: 'Scan',
    help: "Walks the publisher's share, one listing per directory, and records what it has. Transfers nothing; a stopped walk resumes where it left off.",
  },
  {
    kind: 'adopt',
    label: 'Adopt',
    help: 'For files that are already here, such as the USB bootstrap: matches them against the last walk by size alone and records them as present, so a sync does not fetch them again. Scan first.',
  },
  {
    kind: 'sync',
    label: 'Sync',
    help: 'Queues and transfers what the last walk says is missing or changed. A jcc pair then copies its database through the lock gate, and a mirror pair ends with the deletion phase. It does not walk first.',
  },
  {
    kind: 'delete',
    label: 'Delete',
    help: 'The deletion phase on its own: removes files the publisher no longer has. A mirror pair deletes them outright; a guarded pair moves them into .jccmirror/trash, where they are kept for the retention period, and holds them for approval above the threshold. Skipped while transfers are queued or failed. An additive pair never deletes.',
  },
  {
    kind: 'db',
    label: 'Database',
    help: 'Copies ClipCornDB.db on its own, only while neither side holds its .~lock, and keeps the copy it replaces as a backup.',
    jcc: true,
  },
  {
    kind: 'force',
    label: 'force past the lock',
    help: 'Lets Database and Sync copy the database although a .~lock is out. Only right for a lock left behind by a crash.',
    jcc: true,
  },
];

/** Two frames of the stream closer together than this say more about the
 *  stream's cadence than about the link, so the instantaneous rate waits for a
 *  wider gap. Seconds. */
const sampleFloor = 1;

/** NowPage is what is happening and the buttons that make something happen. The
 *  buttons are here rather than in a shell on the Synology because a container's
 *  shell is not somewhere the operator can easily get to (DESIGN.md §4). */
@Component({ selector: 'app-now', imports: [DryRunList], templateUrl: './now.html', styleUrl: './now.css' })
export class NowPage {
  private readonly api = inject(Api);
  private readonly live = inject(Live);
  readonly fmt = fmt;
  readonly actions = actions;

  readonly unlocked = this.api.unlocked;
  readonly loading = signal(false);
  readonly error = signal('');

  readonly pairs = signal<PairView[]>([]);
  readonly status = this.live.status;

  readonly run = computed(() => this.live.runs().current);
  readonly progress = computed(() => this.live.runs().progress);
  readonly scan = computed(() => this.live.runs().scan);
  readonly history = computed(() => this.live.runs().history ?? []);
  readonly busy = computed(() => !!this.run());

  /** The last (bytes, wall clock) the stream reported. The instantaneous rate is
   *  the difference between two of these; the clock is in milliseconds. */
  private readonly markBytes = signal(0);
  private readonly markAt = signal(0);
  readonly instant = signal('-');

  /** Which pairs have the lock-gate override armed. It is per pair and never
   *  sticky beyond the page, because it is only ever a person's answer to one
   *  lock that has gone stale (DESIGN.md §3). */
  private readonly force = signal<Record<number, boolean>>({});

  /** The run the pair table was last read for, so it is re-read once per run
   *  rather than once per frame. */
  private runMark: string | undefined;

  readonly average = computed(() => {
    const p = this.progress();
    if (!p?.startedAt) return '-';
    return fmt.rate(p.runBytes, (Date.now() - Date.parse(p.startedAt)) / 1000);
  });

  /** eta extrapolates from what the run has managed so far. It is not a
   *  prediction: a queue of small files behind a large one moves at a different
   *  rate, and the number moves with it. */
  readonly eta = computed(() => {
    const p = this.progress();
    if (!p?.startedAt || p.runBytes <= 0 || p.runTotal <= p.runBytes) return '-';
    const elapsed = (Date.now() - Date.parse(p.startedAt)) / 1000;
    return fmt.duration(((p.runTotal - p.runBytes) / p.runBytes) * elapsed);
  });

  constructor() {
    this.live.connect();

    effect(() => {
      const p = this.progress();
      untracked(() => this.sample(p));
    });

    // The pair table is three aggregates per pair over the whole manifest, not
    // something to run on every frame. It changes when a run starts and when one
    // finishes, and at no point in between.
    effect(() => {
      const mark = this.live.runs().current?.startedAt ?? '';
      untracked(() => {
        if (mark === this.runMark) return;
        this.runMark = mark;
        void this.load();
      });
    });
  }

  // ---- what is running ----------------------------------------------------

  private sample(p: Progress | undefined): void {
    const now = Date.now();
    if (!p?.startedAt) {
      this.markBytes.set(0);
      this.markAt.set(0);
      this.instant.set('-');
      return;
    }
    // A run that restarted the counter is a new run, not a negative rate.
    if (this.markAt() === 0 || p.runBytes < this.markBytes()) {
      this.markBytes.set(p.runBytes);
      this.markAt.set(now);
      this.instant.set('-');
      return;
    }

    const seconds = (now - this.markAt()) / 1000;
    if (seconds < sampleFloor) return;
    this.instant.set(fmt.rate(p.runBytes - this.markBytes(), seconds));
    this.markBytes.set(p.runBytes);
    this.markAt.set(now);
  }

  elapsed(r: Run): number {
    const end = r.finishedAt ? Date.parse(r.finishedAt) : Date.now();
    return (end - Date.parse(r.startedAt)) / 1000;
  }

  /** Seconds from now until a timestamp the daemon put in the future. */
  until(ts: string | undefined): number {
    if (!ts) return -1;
    return (Date.parse(ts) - Date.now()) / 1000;
  }

  // ---- pairs --------------------------------------------------------------

  help(kind: string): string {
    return actions.find((a) => a.kind === kind)?.help ?? '';
  }

  behind(p: PairView): number {
    const n = p.RemoteFiles - p.LocalFiles;
    return n > 0 ? n : 0;
  }

  queue(p: PairView): { state: string; count: number }[] {
    const counts = p.Queue?.counts ?? {};
    return queueStates
      .filter((state) => counts[state] > 0)
      .map((state) => ({ state, count: counts[state] }));
  }

  backups(p: PairView): DBBackup[] {
    return p.Backups ?? [];
  }

  forced(id: number): boolean {
    return !!this.force()[id];
  }

  setForced(id: number, on: boolean): void {
    this.force.update((m) => ({ ...m, [id]: on }));
  }

  // ---- actions ------------------------------------------------------------

  async start(kind: string, pair: PairView): Promise<void> {
    this.error.set('');
    try {
      await this.api.startRun(kind, pair.id, pair.type === 'jcc' && this.forced(pair.id));
    } catch (err) {
      this.error.set(message(err));
    }
  }

  async stop(): Promise<void> {
    this.error.set('');
    try {
      await this.api.cancelRun();
    } catch (err) {
      this.error.set(message(err));
    }
  }

  async setAutomatic(on: boolean): Promise<void> {
    this.error.set('');
    try {
      await this.api.setAutomatic(on);
    } catch (err) {
      this.error.set(message(err));
    }
  }

  async decide(pair: PairView, decision: 'approved' | 'rejected'): Promise<void> {
    this.error.set('');
    try {
      await this.api.decideDeletion(pair.id, decision);
      await this.load();
    } catch (err) {
      this.error.set(message(err));
    }
  }

  async rollback(pair: PairView, backup: DBBackup): Promise<void> {
    this.error.set('');
    try {
      await this.api.rollbackDatabase(pair.id, backup.id);
      await this.load();
    } catch (err) {
      this.error.set(message(err));
    }
  }

  private async load(): Promise<void> {
    this.loading.set(true);
    try {
      this.pairs.set(await this.api.pairs());
    } catch (err) {
      this.error.set(message(err));
    } finally {
      this.loading.set(false);
    }
  }
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
