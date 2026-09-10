import { Component, computed, inject, signal } from '@angular/core';
import { Api } from '../api';
import { Live } from '../live';
import * as fmt from '../format';
import type { AuditEntry, Cell, ConfigEntry, PairView, ScheduleView } from '../models';

/** The two settings the grids are written in. They are ordinary config keys, so
 *  the field under a grid and the field in the Schedule group are the same draft
 *  entry and save together. */
const transferKey = 'schedule.transfer';
const scanKey = 'schedule.scan';

/** The groups a first boot needs, in the order it needs them. Everything else
 *  keeps the order the registry handed it over in. */
const firstGroups = ['Tunnel', 'Remote', 'Schedule'];

interface Group {
  name: string;
  entries: ConfigEntry[];
}

/** The fields POST /api/pairs accepts, and what a new pair starts as. */
const pairDefaults: Record<string, string> = {
  name: '',
  type: 'raw',
  remotePath: '',
  localPath: '',
  mode: 'additive',
  includes: '',
  excludes: '',
  priority: '100',
  deleteGuard: '0',
  enabled: 'true',
};

/** One pair as the form edits it. Includes and excludes are comma-separated
 *  there and on the wire, which is why a glob cannot contain a comma. */
function pairFields(p: PairView): Record<string, string> {
  return {
    name: p.name,
    type: p.type,
    remotePath: p.remotePath,
    localPath: p.localPath,
    mode: p.mode,
    includes: p.includes.join(', '),
    excludes: p.excludes.join(', '),
    priority: String(p.priority),
    deleteGuard: String(p.deleteGuard),
    enabled: String(p.enabled),
  };
}

/** Which settings are a checkbox rather than a text field. The registry carries
 *  no type, so it is read off the stored value - every boolean key has a
 *  true/false default - with the notification toggles named as well, since a
 *  text field there is the kind of mistake nobody notices until a message does
 *  not arrive. */
function isBoolean(e: ConfigEntry): boolean {
  return !e.secret && (e.value === 'true' || e.value === 'false' || e.label.startsWith('Notify: '));
}

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

/** Config is the whole of DESIGN.md §6: the settings, the two schedules, the
 *  pairs and the trail of what was changed. Nothing here is read from a file or
 *  the environment, so this page is the only way to tell jcc-mirror anything. */
@Component({
  selector: 'app-config',
  imports: [],
  templateUrl: './config.html',
  styleUrl: './config.css',
})
export class ConfigPage {
  private readonly api = inject(Api);
  readonly live = inject(Live);
  readonly fmt = fmt;

  readonly unlocked = this.api.unlocked;
  readonly hours = Array.from({ length: 24 }, (_, h) => h);
  readonly isBoolean = isBoolean;
  readonly transferKey = transferKey;
  readonly scanKey = scanKey;

  readonly loading = signal(false);
  readonly saving = signal(false);
  readonly error = signal('');
  readonly saved = signal<string[]>([]);

  readonly config = signal<ConfigEntry[]>([]);
  readonly schedule = signal<ScheduleView | undefined>(undefined);
  readonly pairs = signal<PairView[]>([]);
  readonly audit = signal<AuditEntry[]>([]);

  /** Only what the operator actually changed. A key that is not in here is not
   *  sent, so a save can never rewrite a setting nobody touched. */
  readonly draft = signal<Record<string, string>>({});
  readonly pending = computed(() => Object.keys(this.draft()).length);

  private readonly byKey = computed(() => new Map(this.config().map((e) => [e.key, e])));

  readonly groups = computed<Group[]>(() => {
    const byName = new Map<string, ConfigEntry[]>();
    for (const entry of this.config()) {
      // The private key is generated and never typed: it never leaves the data
      // volume, and a field for it would only invite pasting one in.
      if (entry.generated) continue;
      const entries = byName.get(entry.group) ?? [];
      entries.push(entry);
      byName.set(entry.group, entries);
    }
    const rank = (name: string) => {
      const i = firstGroups.indexOf(name);
      return i < 0 ? firstGroups.length : i;
    };
    // A stable sort, so the groups that are not spoken for keep the registry's order.
    return [...byName.keys()]
      .sort((a, b) => rank(a) - rank(b))
      .map((name) => ({ name, entries: byName.get(name) ?? [] }));
  });

  /** The live status when there is one, so the outlined cell and the window
   *  cards follow the clock rather than the last fetch. */
  readonly now = computed(() => this.live.status()?.schedule ?? this.schedule()?.now);
  readonly nowHere = computed(() => zoneParts(this.now()?.now));

  // ---- pairs, each with its own draft of only what was edited --------------

  readonly pairEdits = signal<Record<number, Record<string, string>>>({});
  readonly newPair = signal<Record<string, string>>({ ...pairDefaults });
  readonly savingPair = signal(0);
  readonly adding = signal(false);
  /** The pair whose removal is waiting to be confirmed; 0 for none. */
  readonly removing = signal(0);

  // ---- the remote probe ----------------------------------------------------

  readonly probing = signal(false);
  readonly probe = signal<{ url: string; dirs: number; files: number; millis: number } | undefined>(
    undefined,
  );
  readonly probeError = signal('');

  constructor() {
    this.live.connect();
    void this.load();
  }

  async load(): Promise<void> {
    this.loading.set(true);
    try {
      const [config, schedule, pairs, audit] = await Promise.all([
        this.api.config(),
        this.api.schedule(),
        this.api.pairs(),
        this.api.audit(50),
      ]);
      this.config.set(config ?? []);
      this.schedule.set(schedule);
      this.pairs.set(pairs ?? []);
      this.audit.set(audit ?? []);
    } catch (err) {
      this.fail(err);
    } finally {
      this.loading.set(false);
    }
  }

  // ---- the settings form ---------------------------------------------------

  value(entry: ConfigEntry): string {
    return this.draft()[entry.key] ?? entry.value ?? '';
  }

  checked(entry: ConfigEntry): boolean {
    return (this.draft()[entry.key] ?? entry.value) === 'true';
  }

  /** rules is what a grid's field shows: the draft if it was edited, otherwise
   *  the canonical form the daemon printed the grid back as, which says the same
   *  thing as the stored text and says it the same way every time. */
  rules(key: string, canonical: string): string {
    return this.draft()[key] ?? canonical;
  }

  edit(key: string, value: string): void {
    const entry = this.byKey().get(key);
    // A secret is never sent back to us, so its field starts empty and an empty
    // one means "leave it alone" - the same as typing a value back to what is
    // already stored.
    const stored = entry && !entry.secret ? (entry.value ?? '') : '';
    this.draft.update((draft) => {
      const next = { ...draft };
      if (value === stored) delete next[key];
      else next[key] = value;
      return next;
    });
  }

  revert(): void {
    this.draft.set({});
    this.saved.set([]);
  }

  async save(): Promise<void> {
    const values = this.draft();
    if (!Object.keys(values).length) return;

    this.saving.set(true);
    this.error.set('');
    this.saved.set([]);
    try {
      const res = await this.api.setConfig(values);
      this.draft.set({});
      this.saved.set(res.changed ?? []);
      await this.load();
    } catch (err) {
      this.fail(err);
    } finally {
      this.saving.set(false);
    }
  }

  // ---- the grids -----------------------------------------------------------

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

  // ---- pairs ---------------------------------------------------------------

  pairField(p: PairView, field: string): string {
    return this.pairEdits()[p.id]?.[field] ?? pairFields(p)[field] ?? '';
  }

  pairDirty(id: number): boolean {
    return Object.keys(this.pairEdits()[id] ?? {}).length > 0;
  }

  editPair(id: number, field: string, value: string): void {
    this.pairEdits.update((edits) => ({ ...edits, [id]: { ...edits[id], [field]: value } }));
  }

  revertPair(id: number): void {
    this.pairEdits.update((edits) => {
      const next = { ...edits };
      delete next[id];
      return next;
    });
  }

  async savePair(p: PairView): Promise<void> {
    const fields = this.pairEdits()[p.id];
    if (!fields || !Object.keys(fields).length) return;

    this.savingPair.set(p.id);
    this.error.set('');
    try {
      await this.api.updatePair({ id: p.id, ...fields });
      this.revertPair(p.id);
      this.pairs.set((await this.api.pairs()) ?? []);
    } catch (err) {
      this.fail(err);
    } finally {
      this.savingPair.set(0);
    }
  }

  editNew(field: string, value: string): void {
    this.newPair.update((fields) => ({ ...fields, [field]: value }));
  }

  async createPair(): Promise<void> {
    this.adding.set(true);
    this.error.set('');
    try {
      await this.api.createPair(this.newPair());
      this.newPair.set({ ...pairDefaults });
      this.pairs.set((await this.api.pairs()) ?? []);
    } catch (err) {
      this.fail(err);
    } finally {
      this.adding.set(false);
    }
  }

  async removePair(p: PairView): Promise<void> {
    this.error.set('');
    try {
      await this.api.deletePair(p.id);
      this.removing.set(0);
      this.pairs.set((await this.api.pairs()) ?? []);
    } catch (err) {
      this.fail(err);
    }
  }

  // ---- the remote ----------------------------------------------------------

  async testRemote(): Promise<void> {
    this.probing.set(true);
    this.probeError.set('');
    this.probe.set(undefined);
    try {
      const res = await this.api.probeRemote();
      this.probe.set({
        url: String(res['url'] ?? ''),
        dirs: Number(res['dirs'] ?? 0),
        files: Number(res['files'] ?? 0),
        millis: Number(res['millis'] ?? 0),
      });
    } catch (err) {
      this.probeError.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.probing.set(false);
    }
  }

  private fail(err: unknown): void {
    this.error.set(err instanceof Error ? err.message : String(err));
  }
}
