import { Injectable, computed, effect, inject, isDevMode, signal } from '@angular/core';
import { Api } from '../../api';
import type { ConfigEntry } from '../../models';
import { configPages, fallbackPath } from './pages';

export interface ConfigGroup {
  name: string;
  entries: ConfigEntry[];
}

const claimed = new Set(configPages.flatMap((p) => p.groups));

/** ConfigStore is the settings the whole config view shares: one fetch of the
 *  registry that every tab reads, and the one path a save takes. The drafts are
 *  deliberately not here - each page keeps its own (ConfigForm), so a save bar
 *  counts and sends only the fields its own page shows. */
@Injectable({ providedIn: 'root' })
export class ConfigStore {
  private readonly api = inject(Api);

  readonly entries = signal<ConfigEntry[]>([]);
  readonly loading = signal(false);
  readonly error = signal('');

  readonly byKey = computed(() => new Map(this.entries().map((e) => [e.key, e])));

  /** Group name to its fields, in the order the registry handed them over. */
  private readonly byGroup = computed(() => {
    const groups = new Map<string, ConfigEntry[]>();
    for (const entry of this.entries()) {
      const entries = groups.get(entry.group) ?? [];
      entries.push(entry);
      groups.set(entry.group, entries);
    }
    return groups;
  });

  /** The groups the daemon sent that no page in the table asks for. */
  readonly unclaimed = computed(() => [...this.byGroup().keys()].filter((n) => !claimed.has(n)));

  private readonly pageGroups = computed(() => {
    const byGroup = this.byGroup();
    const pages = new Map<string, ConfigGroup[]>();
    for (const page of configPages) {
      const names =
        page.path === fallbackPath ? [...page.groups, ...this.unclaimed()] : page.groups;
      pages.set(
        page.path,
        names
          .filter((n) => byGroup.has(n))
          .map((name) => ({ name, entries: byGroup.get(name) ?? [] })),
      );
    }
    return pages;
  });

  constructor() {
    effect(() => {
      const orphans = this.unclaimed();
      if (!isDevMode() || !orphans.length) return;
      console.warn(
        `configPages claims no page for ${orphans.join(', ')}; rendering on /config/${fallbackPath}`,
      );
    });
  }

  groupsFor(path: string): ConfigGroup[] {
    return this.pageGroups().get(path) ?? [];
  }

  async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      this.entries.set((await this.api.config()) ?? []);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.loading.set(false);
    }
  }

  /** Sends a page's draft and answers with the keys that actually moved. The
   *  reload afterwards is what turns a saved secret's field back into a
   *  placeholder and redraws the schedule grids. */
  async save(values: Record<string, string>): Promise<string[]> {
    const res = await this.api.setConfig(values);
    await this.load();
    return res.changed ?? [];
  }
}
