import { Component, inject, signal } from '@angular/core';
import { Api } from '../../api';
import * as fmt from '../../format';
import type { PairView } from '../../models';

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

/** The pairs editor. A pair is its own record rather than a setting, so this
 *  page saves one pair at a time and has no draft of the settings and no save
 *  bar of its own. */
@Component({
  selector: 'app-config-pairs',
  imports: [],
  templateUrl: './pairs.html',
  styleUrls: ['./page.css', './pairs.css'],
})
export class ConfigPairsPage {
  private readonly api = inject(Api);
  readonly fmt = fmt;
  readonly unlocked = this.api.unlocked;

  readonly pairs = signal<PairView[]>([]);
  readonly error = signal('');

  /** Each pair's own draft of only what was edited. */
  readonly pairEdits = signal<Record<number, Record<string, string>>>({});
  readonly newPair = signal<Record<string, string>>({ ...pairDefaults });
  readonly savingPair = signal(0);
  readonly adding = signal(false);
  /** The pair whose removal is waiting to be confirmed; 0 for none. */
  readonly removing = signal(0);

  constructor() {
    void this.load();
  }

  async load(): Promise<void> {
    try {
      this.pairs.set((await this.api.pairs()) ?? []);
    } catch (err) {
      this.fail(err);
    }
  }

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
      await this.load();
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
      await this.load();
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
      await this.load();
    } catch (err) {
      this.fail(err);
    }
  }

  private fail(err: unknown): void {
    this.error.set(err instanceof Error ? err.message : String(err));
  }
}
