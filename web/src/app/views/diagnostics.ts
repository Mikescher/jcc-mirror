import {
  Component,
  ElementRef,
  afterRenderEffect,
  computed,
  effect,
  inject,
  signal,
  untracked,
  viewChild,
} from '@angular/core';
import { Api } from '../api';
import { Live } from '../live';
import type { DiagnosticsView, LogLine, RemoteListing, Space, UpdateStatus } from '../models';
import * as fmt from '../format';

/** How many lines the tail keeps. It is a tail: older than this is what the
 *  Events view is for. */
const logLines = 1000;

/** What the remote probe answers with. It is a bare map on the wire because the
 *  handler builds the same shape for the event it writes. */
interface Probe {
  target?: string;
  dirs?: number;
  files?: number;
  millis?: number;
}

/** DiagnosticsPage is every question worth asking when the mirror is not
 *  working, in the order they are worth asking them: the tunnel, then the route
 *  through it, then the publisher at the other end (DESIGN.md §4). */
@Component({
  selector: 'app-diagnostics',
  imports: [],
  templateUrl: './diagnostics.html',
  styleUrl: './diagnostics.css',
})
export class DiagnosticsPage {
  private readonly api = inject(Api);
  private readonly live = inject(Live);
  readonly fmt = fmt;

  readonly unlocked = this.api.unlocked;
  readonly loading = signal(false);
  readonly error = signal('');

  readonly view = signal<DiagnosticsView | undefined>(undefined);
  /** The live frame where there is one: the peers' counters and the handshake
   *  age move, and a page that was loaded ten minutes ago should not say so. */
  readonly status = computed(() => this.live.status() ?? this.view()?.status);
  readonly pairs = computed(() => this.view()?.pairs ?? []);
  readonly notify = computed(() => this.view()?.notify ?? []);

  readonly pingTarget = signal('');
  readonly pingResult = signal<{ target: string; millis: number } | undefined>(undefined);
  readonly pingError = signal('');
  readonly pinging = signal(false);

  readonly probePath = signal('');
  readonly probeBytes = signal('');
  readonly throughput = signal<{ path: string; bytes: number; millis: number } | undefined>(
    undefined,
  );
  readonly throughputError = signal('');
  readonly throughputBusy = signal(false);

  readonly reach = signal<Probe | undefined>(undefined);
  readonly reachError = signal('');
  readonly reaching = signal(false);

  readonly path = signal('');
  readonly listing = signal<RemoteListing | undefined>(undefined);
  readonly listError = signal('');
  readonly listBusy = signal(false);

  readonly crumbs = computed(() => {
    const out: { name: string; path: string }[] = [];
    let prefix = '';
    for (const part of this.path().split('/')) {
      if (!part) continue;
      prefix = prefix ? `${prefix}/${part}` : part;
      out.push({ name: part, path: prefix });
    }
    return out;
  });

  readonly explainPair = signal('');
  readonly explainError = signal('');
  /** The newest plan the daemon has run. A plan started from here arrives on the
   *  stream rather than as the answer to the button, because a run answers as
   *  soon as it has started. */
  readonly lastPlan = computed(() =>
    (this.live.runs().history ?? []).find((r) => r.kind === 'plan'),
  );

  readonly update = signal<UpdateStatus | undefined>(undefined);
  readonly updateError = signal('');
  readonly updateBusy = signal(false);
  /** Set once a button has been pressed: the daemon re-execs, so what the page
   *  is waiting for is the same address answering again. */
  readonly restarting = signal(false);

  readonly lines = signal<LogLine[]>([]);
  readonly autoScroll = signal(true);
  private readonly box = viewChild<ElementRef<HTMLElement>>('tail');

  constructor() {
    this.live.connect();
    void this.load();

    effect(() => {
      const incoming = this.live.log();
      untracked(() => this.append(incoming));
    });

    afterRenderEffect(() => {
      this.lines();
      const el = this.box()?.nativeElement;
      if (el && untracked(this.autoScroll)) el.scrollTop = el.scrollHeight;
    });
  }

  // ---- the tunnel and the pairs ------------------------------------------

  used(space: Space): number {
    return fmt.percent(space.total - space.free, space.total);
  }

  pairName(id: number): string {
    return this.pairs().find((p) => p.id === id)?.name ?? '—';
  }

  level(line: LogLine): string {
    switch (line.level) {
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

  // ---- the probes ---------------------------------------------------------

  async ping(): Promise<void> {
    this.pinging.set(true);
    this.pingError.set('');
    try {
      this.pingResult.set(await this.api.ping(this.pingTarget().trim()));
    } catch (err) {
      this.pingResult.set(undefined);
      this.pingError.set(message(err));
    } finally {
      this.pinging.set(false);
    }
  }

  async measure(): Promise<void> {
    this.throughputBusy.set(true);
    this.throughputError.set('');
    try {
      const want = Number(this.probeBytes().trim());
      this.throughput.set(
        await this.api.throughput(this.probePath().trim(), want > 0 ? want : undefined),
      );
    } catch (err) {
      this.throughput.set(undefined);
      this.throughputError.set(message(err));
    } finally {
      this.throughputBusy.set(false);
    }
  }

  async probe(): Promise<void> {
    this.reaching.set(true);
    this.reachError.set('');
    try {
      this.reach.set((await this.api.probeRemote()) as Probe);
    } catch (err) {
      this.reach.set(undefined);
      this.reachError.set(message(err));
    } finally {
      this.reaching.set(false);
    }
  }

  async open(path: string): Promise<void> {
    this.path.set(path);
    this.listBusy.set(true);
    this.listError.set('');
    try {
      this.listing.set(await this.api.remoteList(path));
    } catch (err) {
      this.listError.set(message(err));
    } finally {
      this.listBusy.set(false);
    }
  }

  async explain(): Promise<void> {
    this.explainError.set('');
    const id = Number(this.explainPair());
    if (!id) {
      this.explainError.set('Choose a pair first.');
      return;
    }
    try {
      await this.api.startRun('plan', id);
    } catch (err) {
      this.explainError.set(message(err));
    }
  }

  // ---- the self-updater ---------------------------------------------------

  async checkUpdate(): Promise<void> {
    this.updateBusy.set(true);
    this.updateError.set('');
    try {
      this.update.set(await this.api.checkUpdate());
    } catch (err) {
      this.updateError.set(message(err));
    } finally {
      this.updateBusy.set(false);
    }
  }

  async applyUpdate(force = false): Promise<void> {
    this.updateBusy.set(true);
    this.updateError.set('');
    try {
      await this.api.applyUpdate(force);
      this.restarting.set(true);
      await this.awaitRestart();
    } catch (err) {
      this.updateError.set(message(err));
    } finally {
      this.updateBusy.set(false);
    }
  }

  async rollbackUpdate(): Promise<void> {
    this.updateBusy.set(true);
    this.updateError.set('');
    try {
      await this.api.rollbackUpdate();
      this.restarting.set(true);
      await this.awaitRestart();
    } catch (err) {
      this.updateError.set(message(err));
    } finally {
      this.updateBusy.set(false);
    }
  }

  /** The daemon re-execs into the new binary, which takes about as long as a
   *  process start. Polling the panel back is what turns that into "it came back
   *  as v2" rather than a page that has to be reloaded by hand. */
  private async awaitRestart(): Promise<void> {
    for (let attempt = 0; attempt < 30; attempt++) {
      await new Promise((done) => setTimeout(done, 1000));
      try {
        this.update.set(await this.api.update());
        this.restarting.set(false);
        void this.load();
        return;
      } catch {
        // Still down, or halfway through binding its listeners again.
      }
    }
    this.restarting.set(false);
    this.updateError.set(
      'The daemon has not answered since it restarted. Check the container log.',
    );
  }

  // ---- the log tail -------------------------------------------------------

  private seed(lines: LogLine[] | undefined): void {
    this.lines.update((have) => {
      const first = have.length ? have[0].seq : Number.MAX_SAFE_INTEGER;
      const older = (lines ?? []).filter((l) => l.seq < first);
      return older.length ? [...older, ...have].slice(-logLines) : have;
    });
  }

  private append(lines: LogLine[]): void {
    if (lines.length === 0) return;
    this.lines.update((have) => {
      const last = have.length ? have[have.length - 1].seq : 0;
      const fresh = lines.filter((l) => l.seq > last);
      return fresh.length ? [...have, ...fresh].slice(-logLines) : have;
    });
  }

  async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      const view = await this.api.diagnostics();
      this.view.set(view);
      this.seed(view.log ?? []);
      this.update.set(await this.api.update());
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
