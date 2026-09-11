import { Component, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { Api } from '../../api';
import { Live } from '../../live';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** The tunnel every remote is reached through, and the wg-quick importer that
 *  fills its fields in one paste. */
@Component({
  selector: 'app-config-connection',
  imports: [ConfigGroupCard, ConfigSaveBar, RouterLink],
  templateUrl: './connection.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigConnectionPage {
  private readonly api = inject(Api);
  private readonly store = inject(ConfigStore);
  readonly live = inject(Live);
  readonly unlocked = this.api.unlocked;

  readonly groups = computed(() => this.store.groupsFor('connection'));

  readonly importText = signal('');
  readonly importing = signal(false);
  readonly importError = signal('');
  readonly imported = signal<string[] | undefined>(undefined);
  readonly importIgnored = signal<string[]>([]);

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
}
