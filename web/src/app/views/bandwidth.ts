import { Component, computed, effect, inject, signal } from '@angular/core';
import { Api } from '../api';
import type { BandwidthView } from '../models';
import * as fmt from '../format';

type Span = 'minute' | 'hour' | 'day';

/** Seconds in one bucket at each resolution. A bucket holds bytes; a rate needs
 *  to know how long the bucket was. */
const bucketSeconds: Record<Span, number> = { minute: 60, hour: 3600, day: 86400 };

/** The most columns the chart draws. A week of minutes is 10 080 buckets, which
 *  is more rects than the chart has pixels to put them in, so neighbours are
 *  folded together until they fit and a column's title says the range it covers. */
const maxColumns = 240;

/** The viewBox is this tall, which makes a gridline's y coordinate and the
 *  percentage its HTML label is positioned at the same number. */
const chartHeight = 100;

const dayNames = ['Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday', 'Sunday'];

interface Bar {
  x: number;
  y: number;
  h: number;
  title: string;
}

interface Gridline {
  y: number;
  value: number;
}

/** BandwidthPage is what the link actually carried: a series at one of the three
 *  resolutions the samples are kept at, and the hour-of-day shape of it
 *  (DESIGN.md §4). The chart is hand-drawn SVG - the page carries no charting
 *  library and is not to acquire one. */
@Component({
  selector: 'app-bandwidth',
  imports: [],
  templateUrl: './bandwidth.html',
  styleUrl: './bandwidth.css',
})
export class BandwidthPage {
  private readonly api = inject(Api);
  readonly fmt = fmt;
  readonly chartHeight = chartHeight;
  readonly barWidth = 0.85;
  readonly hours = Array.from({ length: 24 }, (_, h) => h);
  readonly days = dayNames.map((d) => d.slice(0, 3));
  readonly legendSteps = [0, 0.25, 0.5, 0.75, 1];

  readonly span = signal<Span>('minute');
  readonly view = signal<BandwidthView | undefined>(undefined);
  readonly loading = signal(false);
  readonly error = signal('');

  private seq = 0;

  /** The bucket the live rate is quoted over, taken from the answer rather than
   *  the selector so a reload in flight cannot divide by the wrong number. */
  readonly bucket = computed(() => bucketSeconds[this.view()?.span ?? 'minute']);

  /** The window the average rate is taken over: from the first sample to now,
   *  not the nominal retention. The day span reports no lower bound at all, and
   *  a series that only starts halfway through its window would otherwise
   *  average as though the half before it had been quiet. */
  readonly windowSeconds = computed(() => {
    const view = this.view();
    if (!view || view.samples.length === 0) return 0;
    const first = Date.parse(view.samples[0].ts);
    const to = Date.parse(view.to);
    return Math.max(bucketSeconds[view.span], (to - first) / 1000);
  });

  /** The series geometry, or undefined when there is nothing to draw. Bars are
   *  bytes in; out is left to the totals rather than drawn, because asking for a
   *  file costs three orders of magnitude less than receiving it and a second
   *  series on the same scale would be a row of invisible marks. */
  readonly chart = computed(() => {
    const samples = this.view()?.samples ?? [];
    if (samples.length === 0) return undefined;

    const fold = Math.ceil(samples.length / maxColumns);
    const columns: { from: string; to: string; in: number; out: number }[] = [];
    for (let i = 0; i < samples.length; i += fold) {
      const group = samples.slice(i, i + fold);
      let bytesIn = 0;
      let bytesOut = 0;
      for (const s of group) {
        bytesIn += s.in;
        bytesOut += s.out;
      }
      columns.push({
        from: group[0].ts,
        to: group[group.length - 1].ts,
        in: bytesIn,
        out: bytesOut,
      });
    }

    const max = Math.max(1, ...columns.map((c) => c.in));
    const bars: Bar[] = columns.map((c, i) => {
      const h = (c.in / max) * chartHeight;
      const when = fold > 1 ? `${fmt.clock(c.from)} – ${fmt.timeOnly(c.to)}` : fmt.clock(c.from);
      return {
        x: i + (1 - this.barWidth) / 2,
        y: chartHeight - h,
        h,
        title: `${when} · ${fmt.bytes(c.in)} in · ${fmt.bytes(c.out)} out`,
      };
    });

    const gridlines: Gridline[] = [1, 2 / 3, 1 / 3].map((f) => ({
      y: chartHeight * (1 - f),
      value: max * f,
    }));

    return { bars, gridlines, width: columns.length, fold, buckets: samples.length };
  });

  readonly heatmap = computed(() => this.view()?.heatmap ?? []);
  readonly heatMax = computed(() => Math.max(0, ...this.heatmap().flat()));

  /** The daily series has no hour-of-day left in it, so the daemon sends the grid
   *  empty rather than sending zeroes that would read as an idle week. */
  readonly hasHeatmap = computed(() => this.view()?.span !== 'day' && this.heatMax() > 0);

  constructor() {
    effect(() => void this.load(this.span()));
  }

  async reload(): Promise<void> {
    await this.load(this.span());
  }

  /** The share of the busiest hour, as a shade. A floor of 6%: an hour with
   *  something in it must not draw the same as an hour with nothing. */
  shade(bytesIn: number): string | null {
    const max = this.heatMax();
    if (bytesIn <= 0 || max <= 0) return null;
    return mix(Math.max(6, Math.round((bytesIn / max) * 100)));
  }

  legendShade(fraction: number): string | null {
    return fraction <= 0 ? null : mix(Math.round(fraction * 100));
  }

  cellTitle(day: number, hour: number, bytesIn: number): string {
    return `${dayNames[day]} ${String(hour).padStart(2, '0')}:00 · ${fmt.bytes(bytesIn)} in`;
  }

  private async load(span: Span): Promise<void> {
    const seq = ++this.seq;
    this.loading.set(true);
    try {
      const view = await this.api.bandwidth(span);
      if (seq !== this.seq) return;
      this.view.set(view);
      this.error.set('');
    } catch (err) {
      if (seq === this.seq) this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      if (seq === this.seq) this.loading.set(false);
    }
  }
}

function mix(percent: number): string {
  return `color-mix(in srgb, var(--accent) ${percent}%, transparent)`;
}
