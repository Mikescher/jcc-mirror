import { Component, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import { Live } from '../../live';
import * as fmt from '../../format';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** The tunnel and the share behind it, plus the two things that answer whether
 *  they work: the wg-quick importer that fills the tunnel fields in one paste,
 *  and a single listing of the remote root. */
@Component({
  selector: 'app-config-connection',
  imports: [ConfigGroupCard, ConfigSaveBar],
  templateUrl: './connection.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigConnectionPage {
  private readonly api = inject(Api);
  private readonly store = inject(ConfigStore);
  readonly live = inject(Live);
  readonly fmt = fmt;
  readonly unlocked = this.api.unlocked;

  readonly groups = computed(() => this.store.groupsFor('connection'));

  readonly importText = signal('');
  readonly importing = signal(false);
  readonly importError = signal('');
  readonly imported = signal<string[] | undefined>(undefined);
  readonly importIgnored = signal<string[]>([]);

  readonly probing = signal(false);
  readonly probe = signal<
    { target: string; dirs: number; files: number; millis: number } | undefined
  >(undefined);
  readonly probeError = signal('');

  async importWireguard(): Promise<void> {
    const config = this.importText();
    if (!config.trim()) return;

    this.importing.set(true);
    this.importError.set('');
    this.imported.set(undefined);
    this.importIgnored.set([]);
    try {
      const res = await this.api.importWireguard(config);
      this.imported.set(res.changed ?? []);
      this.importIgnored.set(res.ignored ?? []);
      this.importText.set('');
      // The fields below are now the imported values, not what was fetched
      // before the paste.
      await this.store.load();
    } catch (err) {
      this.importError.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.importing.set(false);
    }
  }

  async testRemote(): Promise<void> {
    this.probing.set(true);
    this.probeError.set('');
    this.probe.set(undefined);
    try {
      const res = await this.api.probeRemote();
      this.probe.set({
        target: String(res['target'] ?? ''),
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
}
