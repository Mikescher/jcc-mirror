import { Component, computed, effect, inject, signal } from '@angular/core';
import { Api } from '../api';
import type { BandwidthDay, BandwidthView } from '../models';
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
  label: string;
}

interface CalendarDay {
  date: string;
  title: string;
  shade: string | null;
  today: boolean;
  future: boolean;
}

interface CalendarMonth {
  label: string;
  totalIn: number;
  /** Blank cells before the 1st, so the 1st lands under its weekday. */
  lead: number[];
  days: CalendarDay[];
}

/** BandwidthPage is what the link actually carried: a series at one of the three
 *  resolutions the samples are kept at, the hour-of-day shape of it, and a
 *  calendar of every day on record (DESIGN.md §4). The chart is hand-drawn SVG - the page carries no charting
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
  readonly weekdayInitials = dayNames.map((d) => d[0]);
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
   *  bytes in per second; out is left to the totals rather than drawn, because
   *  asking for a file costs three orders of magnitude less than receiving it
   *  and a second series on the same scale would be a row of invisible marks.
   *  A bucket in which nothing moved has no row, so a column's rate is over the
   *  buckets it holds: the speed while the link was busy. A day has no busy time
   *  left in it to divide by, so the day span draws bytes per day instead. */
  readonly chart = computed(() => {
    const view = this.view();
    const samples = view?.samples ?? [];
    if (!view || samples.length === 0) return undefined;
    const bucket = bucketSeconds[view.span];
    const perDay = view.span === 'day';

    const fold = Math.ceil(samples.length / maxColumns);
    const columns: { from: string; to: string; in: number; out: number; value: number }[] = [];
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
        value: perDay ? bytesIn / group.length : bytesIn / (group.length * bucket),
      });
    }

    const label = (value: number) => (perDay ? `${fmt.bytes(value)}/day` : fmt.speed(value));
    const max = Math.max(1, ...columns.map((c) => c.value));
    const bars: Bar[] = columns.map((c, i) => {
      const h = (c.value / max) * chartHeight;
      let when: string;
      if (perDay) {
        when = fold > 1 ? `${fmt.dateOnly(c.from)} – ${fmt.dateOnly(c.to)}` : fmt.dateOnly(c.from);
      } else {
        when = fold > 1 ? `${fmt.clock(c.from)} – ${fmt.timeOnly(c.to)}` : fmt.clock(c.from);
      }
      return {
        x: i + (1 - this.barWidth) / 2,
        y: chartHeight - h,
        h,
        title: `${when} · ${label(c.value)} · ${fmt.bytes(c.in)} in · ${fmt.bytes(c.out)} out`,
      };
    });

    const gridlines: Gridline[] = [1, 2 / 3, 1 / 3].map((f) => ({
      y: chartHeight * (1 - f),
      label: label(max * f),
    }));

    return { bars, gridlines, width: columns.length, fold, perDay, buckets: samples.length };
  });

  readonly heatmap = computed(() => this.view()?.heatmap ?? []);

  /** [weekday][hour] bytes in per second while busy, over the samples behind
   *  each cell. The cells are hours whatever the span, but a sample is one
   *  bucket of the span it was read at. */
  readonly heatSpeed = computed(() => {
    const view = this.view();
    if (!view) return [];
    const bucket = bucketSeconds[view.span];
    return view.heatmap.map((row, d) =>
      row.map((bytesIn, h) => {
        const n = view.heatmapBuckets?.[d]?.[h] ?? 0;
        return n > 0 ? bytesIn / (n * bucket) : 0;
      }),
    );
  });
  readonly heatMax = computed(() => Math.max(0, ...this.heatSpeed().flat()));

  /** The daily series has no hour-of-day left in it, so the daemon sends the grid
   *  empty rather than sending zeroes that would read as an idle week. */
  readonly hasHeatmap = computed(() => this.view()?.span !== 'day' && this.heatMax() > 0);

  /** The busiest day's bytes in, which the calendar shades against. */
  readonly calendarMax = computed(() => Math.max(0, ...(this.view()?.days ?? []).map((d) => d.in)));

  /** Every month from the first day on record to the current one, laid out
   *  Monday first. Dates are YYYY-MM-DD in the configured timezone and are only
   *  ever handled as UTC calendar dates here, so the browser's own zone cannot
   *  shift one onto its neighbour. */
  readonly calendar = computed((): CalendarMonth[] => {
    const view = this.view();
    const days = view?.days ?? [];
    if (!view || days.length === 0) return [];
    const byDate = new Map<string, BandwidthDay>(days.map((d) => [d.date, d]));
    const max = this.calendarMax();

    const [ty, tm] = view.today.split('-').map(Number);
    let [y, m] = days[0].date.split('-').map(Number);
    const months: CalendarMonth[] = [];
    while (y < ty || (y === ty && m <= tm)) {
      const first = new Date(Date.UTC(y, m - 1, 1));
      const count = new Date(Date.UTC(y, m, 0)).getUTCDate();
      const month: CalendarMonth = {
        label: first.toLocaleDateString('en-US', {
          month: 'long',
          year: 'numeric',
          timeZone: 'UTC',
        }),
        totalIn: 0,
        lead: Array.from({ length: (first.getUTCDay() + 6) % 7 }, (_, i) => i),
        days: [],
      };
      for (let d = 1; d <= count; d++) {
        const date = `${y}-${pad(m)}-${pad(d)}`;
        const entry = byDate.get(date);
        const bytesIn = entry?.in ?? 0;
        month.totalIn += bytesIn;
        const weekday = dayNames[(new Date(Date.UTC(y, m - 1, d)).getUTCDay() + 6) % 7];
        month.days.push({
          date,
          title: `${weekday} ${date} · ${fmt.bytes(bytesIn)} in · ${fmt.bytes(entry?.out ?? 0)} out`,
          shade:
            bytesIn > 0 && max > 0 ? mix(Math.max(6, Math.round((bytesIn / max) * 100))) : null,
          today: date === view.today,
          future: date > view.today,
        });
      }
      months.push(month);
      if (++m > 12) {
        m = 1;
        y++;
      }
    }
    return months;
  });

  constructor() {
    effect(() => void this.load(this.span()));
  }

  async reload(): Promise<void> {
    await this.load(this.span());
  }

  /** The share of the fastest hour, as a shade. A floor of 6%: an hour with
   *  something in it must not draw the same as an hour with nothing. */
  shade(speed: number): string | null {
    const max = this.heatMax();
    if (speed <= 0 || max <= 0) return null;
    return mix(Math.max(6, Math.round((speed / max) * 100)));
  }

  legendShade(fraction: number): string | null {
    return fraction <= 0 ? null : mix(Math.round(fraction * 100));
  }

  cellTitle(day: number, hour: number): string {
    const when = `${dayNames[day]} ${String(hour).padStart(2, '0')}:00`;
    const bytesIn = this.heatmap()[day]?.[hour] ?? 0;
    return `${when} · ${fmt.speed(this.heatSpeed()[day]?.[hour] ?? 0)} · ${fmt.bytes(bytesIn)} in`;
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

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

function mix(percent: number): string {
  return `color-mix(in srgb, var(--accent) ${percent}%, transparent)`;
}
