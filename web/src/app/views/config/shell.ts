import { Component, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterOutlet } from '@angular/router';
import { type ConfigPage, configPage } from './pages';
import { ConfigStore } from './store';

/** The frame every settings page renders in: its title, the sentence that says
 *  what it decides, and the outlet. The settings themselves belong to the
 *  store, so moving between these pages costs no request. */
@Component({
  selector: 'app-config-shell',
  imports: [RouterOutlet],
  templateUrl: './shell.html',
  styleUrl: './shell.css',
})
export class ConfigShell {
  private readonly route = inject(ActivatedRoute);
  readonly store = inject(ConfigStore);

  readonly page = signal<ConfigPage | undefined>(undefined);

  constructor() {
    // Once per arrival from a live view rather than once per page: the shell
    // outlives the pages inside it.
    void this.store.load();
  }

  /** The outlet is the only thing that knows which child is up. */
  activated(): void {
    this.page.set(configPage(this.route.firstChild?.snapshot.routeConfig?.path ?? ''));
  }
}
