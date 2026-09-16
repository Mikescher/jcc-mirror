import { Injectable, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import type { ConfigEntry } from '../../models';
import { ConfigStore } from './store';

/** Which settings are a checkbox rather than a text field. The registry carries
 *  no type, so it is read off the stored value: every boolean key has a
 *  true/false default. */
export function isBoolean(e: ConfigEntry): boolean {
  return !e.secret && (e.value === 'true' || e.value === 'false');
}

/** ConfigForm is one page's draft. Every config page provides its own, which is
 *  what keeps the save bars honest: each holds only the keys its page can
 *  edit, so saving here can never rewrite a field on another tab. */
@Injectable()
export class ConfigForm {
  private readonly store = inject(ConfigStore);
  readonly unlocked = inject(Api).unlocked;

  /** Only what the operator actually changed. A key that is not in here is not
   *  sent, so a save can never rewrite a setting nobody touched. */
  readonly draft = signal<Record<string, string>>({});
  readonly pending = computed(() => Object.keys(this.draft()).length);

  readonly saving = signal(false);
  readonly error = signal('');
  readonly saved = signal<string[]>([]);

  value(entry: ConfigEntry): string {
    return this.draft()[entry.key] ?? entry.value ?? '';
  }

  checked(entry: ConfigEntry): boolean {
    return (this.draft()[entry.key] ?? entry.value) === 'true';
  }

  /** What a schedule grid's field shows: the draft if it was edited, otherwise
   *  the rules as they were typed. Only an empty setting falls back to the form
   *  the daemon printed the grid back as, one rule per line; a rule never
   *  contains a semicolon, so splitting on them is safe. */
  rules(key: string, canonical: string): string {
    const stored = this.store.byKey().get(key)?.value;
    return this.draft()[key] ?? (stored || canonical.split(/\s*;\s*/).join('\n'));
  }

  edit(key: string, value: string): void {
    const entry = this.store.byKey().get(key);
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
      const changed = await this.store.save(values);
      this.draft.set({});
      this.saved.set(changed);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.saving.set(false);
    }
  }
}
