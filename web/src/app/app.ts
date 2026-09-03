import { Component, computed, inject, signal } from '@angular/core';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { Api } from './api';
import { Live } from './live';
import * as fmt from './format';

/** App is the shell: the six views' navigation, the one line of status that is
 *  worth seeing from every one of them, and the token unlock. Everything below it
 *  reads the same live stream. */
@Component({
  selector: 'app-root',
  imports: [RouterOutlet, RouterLink, RouterLinkActive],
  templateUrl: './app.html',
  styleUrl: './app.css',
})
export class App {
  private readonly api = inject(Api);
  readonly live = inject(Live);
  readonly fmt = fmt;

  readonly authed = this.api.authed;
  readonly token = signal('');
  readonly error = signal('');
  readonly unlocking = signal(false);

  readonly status = this.live.status;
  readonly busy = computed(() => !!this.live.runs().current);

  readonly tunnel = computed(() => {
    const state = this.status()?.tunnel.state;
    switch (state) {
      case 'up':
        return { label: 'tunnel up', klass: 'ok' };
      case 'connecting':
        return { label: 'tunnel connecting', klass: 'warn' };
      case 'failed':
        return { label: 'tunnel failed', klass: 'bad' };
      default:
        return { label: 'tunnel not configured', klass: 'dim' };
    }
  });

  constructor() {
    this.live.connect();
    void this.api.session();
  }

  async unlock(event: Event): Promise<void> {
    event.preventDefault();
    this.unlocking.set(true);
    this.error.set('');
    try {
      await this.api.login(this.token());
      this.token.set('');
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.unlocking.set(false);
    }
  }

  async lock(): Promise<void> {
    await this.api.logout();
  }
}
