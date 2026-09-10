import { Component, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** Where SimpleCloudNotify sends and what is worth sending, with the one button
 *  that proves the keys are right. */
@Component({
  selector: 'app-config-notifications',
  imports: [ConfigGroupCard, ConfigSaveBar],
  templateUrl: './notifications.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigNotificationsPage {
  private readonly api = inject(Api);
  private readonly store = inject(ConfigStore);
  readonly unlocked = this.api.unlocked;

  readonly groups = computed(() => this.store.groupsFor('notifications'));

  readonly sending = signal(false);
  readonly sent = signal(false);
  readonly sendError = signal('');

  async sendTest(): Promise<void> {
    this.sending.set(true);
    this.sent.set(false);
    this.sendError.set('');
    try {
      await this.api.testNotification();
      this.sent.set(true);
    } catch (err) {
      this.sendError.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.sending.set(false);
    }
  }
}
