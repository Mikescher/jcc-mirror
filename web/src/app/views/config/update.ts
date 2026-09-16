import { Component, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import * as fmt from '../../format';
import type { UpdateStatus } from '../../models';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** The self-updater: where a binary comes from, and the buttons that install
 *  one or step back off it (DESIGN.md §5). */
@Component({
  selector: 'app-config-update',
  imports: [ConfigGroupCard, ConfigSaveBar],
  templateUrl: './update.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigUpdatePage {
  private readonly api = inject(Api);
  private readonly store = inject(ConfigStore);
  readonly fmt = fmt;
  readonly unlocked = this.api.unlocked;

  readonly groups = computed(() => this.store.groupsFor('update'));

  readonly update = signal<UpdateStatus | undefined>(undefined);
  readonly error = signal('');
  readonly busy = signal(false);
  /** Set once a button has been pressed: the daemon re-execs, so what the page
   *  is waiting for is the same address answering again. */
  readonly restarting = signal(false);

  constructor() {
    void this.load();
  }

  async load(): Promise<void> {
    try {
      this.update.set(await this.api.update());
    } catch (err) {
      this.error.set(message(err));
    }
  }

  async check(): Promise<void> {
    await this.act(async () => this.update.set(await this.api.checkUpdate()));
  }

  async apply(force = false): Promise<void> {
    await this.act(async () => {
      await this.api.applyUpdate(force);
      await this.awaitRestart();
    });
  }

  async rollback(): Promise<void> {
    await this.act(async () => {
      await this.api.rollbackUpdate();
      await this.awaitRestart();
    });
  }

  private async act(fn: () => Promise<void>): Promise<void> {
    this.busy.set(true);
    this.error.set('');
    try {
      await fn();
    } catch (err) {
      this.error.set(message(err));
    } finally {
      this.busy.set(false);
    }
  }

  /** The daemon re-execs into the new binary, which takes about as long as a
   *  process start. Polling the panel back is what turns that into "it came back
   *  as v2" rather than a page that has to be reloaded by hand. */
  private async awaitRestart(): Promise<void> {
    this.restarting.set(true);
    try {
      for (let attempt = 0; attempt < 30; attempt++) {
        await new Promise((done) => setTimeout(done, 1000));
        try {
          this.update.set(await this.api.update());
          return;
        } catch {
          // Still down, or halfway through binding its listeners again.
        }
      }
      throw new Error('The daemon has not answered since it restarted. Check the container log.');
    } finally {
      this.restarting.set(false);
    }
  }
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
