import { Component, computed, inject } from '@angular/core';
import { RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { Api } from './api';
import { Live } from './live';
import { configPages } from './views/config/pages';
import * as fmt from './format';

/** App is the shell: the navigation over the live views and the settings pages, the one line of status that is
 *  worth seeing from every one of them, and the read-only toggle. Everything
 *  below it reads the same live stream. */
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
  readonly pages = configPages;

  readonly unlocked = this.api.unlocked;

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

  readonly remotes = computed(() => this.status()?.remotes ?? []);
  /** One `name: error` line per remote that is failing; empty when none is. */
  readonly unreachable = computed(() =>
    this.remotes()
      .filter((r) => r.error)
      .map((r) => `${r.name}: ${r.error}`)
      .join('\n'),
  );

  constructor() {
    this.live.connect();
  }

  toggleLock(): void {
    this.api.toggleUnlocked();
  }
}
