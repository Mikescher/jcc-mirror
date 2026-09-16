import {
  Component,
  ElementRef,
  afterRenderEffect,
  effect,
  inject,
  input,
  signal,
  untracked,
  viewChild,
} from '@angular/core';
import { Live } from '../../live';
import type { LogLine } from '../../models';
import * as fmt from '../../format';

/** How many lines the tail keeps. It is a tail: older than this is what the
 *  Events view is for. */
const logLines = 1000;

/** The daemon's log: what it had when the page loaded, and every line since. */
@Component({
  selector: 'config-log-tail',
  imports: [],
  templateUrl: './log-tail.html',
  styleUrls: ['./page.css', './log-tail.css'],
})
export class ConfigLogTail {
  private readonly live = inject(Live);
  readonly fmt = fmt;

  /** The lines GET /api/diagnostics had, which predate the stream. */
  readonly seed = input<LogLine[] | undefined>(undefined);

  readonly lines = signal<LogLine[]>([]);
  readonly follow = signal(true);
  private readonly box = viewChild<ElementRef<HTMLElement>>('tail');

  constructor() {
    this.live.connect();

    effect(() => {
      const older = this.seed() ?? [];
      untracked(() => this.prepend(older));
    });

    effect(() => {
      const incoming = this.live.log();
      untracked(() => this.append(incoming));
    });

    afterRenderEffect(() => {
      this.lines();
      const el = this.box()?.nativeElement;
      if (el && untracked(this.follow)) el.scrollTop = el.scrollHeight;
    });
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

  private prepend(lines: LogLine[]): void {
    this.lines.update((have) => {
      const first = have.length ? have[0].seq : Number.MAX_SAFE_INTEGER;
      const older = lines.filter((l) => l.seq < first);
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
}
