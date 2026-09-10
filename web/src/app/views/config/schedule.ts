import { Component, computed, effect, inject, signal } from '@angular/core';
import { Api } from '../../api';
import { Live } from '../../live';
import * as fmt from '../../format';
import type { Cell, ScheduleView } from '../../models';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** The two settings the grids are written in. They are ordinary config keys, so
 *  the field under a grid and the field in the Schedule group are the same draft
 *  entry and save together. */
const transferKey = 'schedule.transfer';
const scanKey = 'schedule.scan';

interface ZoneParts {
  weekday: number;
  hour: number;
  text: string;
}

/** zoneParts reads a timestamp's own fields. The daemon writes them with the
 *  configured zone's offset, and Date would answer in the browser's zone - a
 *  different hour, and near midnight a different weekday, than the grid is drawn
 *  in. */
function zoneParts(ts: string | undefined): ZoneParts | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(ts ?? '');
  if (!m) return undefined;
  const [, year, month, day, hour, minute] = m;
  return {
    // getUTCDay counts from Sunday; the grids are Monday first.
    weekday: (new Date(Date.UTC(+year, +month - 1, +day)).getUTCDay() + 6) % 7,
    hour: +hour,
    text: `${year}-${month}-${day} ${hour}:${minute}`,
  };
}

/** When transfers and scans may run. The two 7x24 grids are the daemon's own
 *  reading of the rules below them, which is the point of showing both: a rule
 *  that does not say what it looks like says so here. */
@Component({
  selector: 'app-config-schedule',
  imports: [ConfigGroupCard, ConfigSaveBar],
  templateUrl: './schedule.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigSchedulePage {
  private readonly api = inject(Api);
  private readonly store = inject(ConfigStore);
  readonly live = inject(Live);
  readonly form = inject(ConfigForm);
  readonly fmt = fmt;

  readonly unlocked = this.api.unlocked;
  readonly hours = Array.from({ length: 24 }, (_, h) => h);
  readonly transferKey = transferKey;
  readonly scanKey = scanKey;

  readonly groups = computed(() => this.store.groupsFor('schedule'));
  readonly schedule = signal<ScheduleView | undefined>(undefined);

  /** The live status when there is one, so the outlined cell and the window
   *  cards follow the clock rather than the last fetch. */
  readonly now = computed(() => this.live.status()?.schedule ?? this.schedule()?.now);
  readonly nowHere = computed(() => zoneParts(this.now()?.now));

  constructor() {
    // The daemon draws the grids from the stored rules, so they are re-fetched
    // rather than recomputed here - including after a save, which is what the
    // settings landing is the signal for.
    effect(() => {
      if (this.store.entries().length) void this.load();
    });
  }

  private async load(): Promise<void> {
    try {
      this.schedule.set(await this.api.schedule());
    } catch {
      // The shell's banner already carries whatever the settings fetch said, and
      // a schedule that will not load leaves the grids out rather than the page.
    }
  }

  cellClass(cell: Cell, day: number, hour: number): string {
    const state = !cell.open ? 'cell-off' : cell.limit ? 'cell-cap' : 'cell-full';
    const at = this.nowHere();
    return at && at.weekday === day && at.hour === hour ? state + ' cell-now' : state;
  }

  cellTitle(days: string[], day: number, hour: number, cell: Cell, gate = false): string {
    const when = `${days[day] ?? ''} ${String(hour).padStart(2, '0')}:00`;
    if (!cell.open) return `${when} — closed`;
    return gate ? `${when} — open` : `${when} — ${fmt.cap(cell.limit)}`;
  }
}
