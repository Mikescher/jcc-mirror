import { Component, inject, signal } from '@angular/core';
import { ActivatedRoute, RouterLink, RouterLinkActive, RouterOutlet } from '@angular/router';
import { type ConfigPage, configPage, configPages } from './pages';
import { ConfigStore } from './store';

/** The config view is one tab per page (DESIGN.md §6, split up): this holds the
 *  tabs, the sentence that says what the open one decides, and the outlet the
 *  pages render into. The settings themselves belong to the store, so moving
 *  between tabs costs no request. */
@Component({
  selector: 'app-config-shell',
  imports: [RouterOutlet, RouterLink, RouterLinkActive],
  templateUrl: './shell.html',
  styleUrl: './shell.css',
})
export class ConfigShell {
  private readonly route = inject(ActivatedRoute);
  readonly store = inject(ConfigStore);
  readonly pages = configPages;

  readonly page = signal<ConfigPage | undefined>(undefined);

  constructor() {
    // Once per visit to the config view rather than once per tab: the shell
    // outlives the pages inside it.
    void this.store.load();
  }

  /** The outlet is the only thing that knows which child is up; the tabs are
   *  routerLinks and answer for themselves. */
  activated(): void {
    this.page.set(configPage(this.route.firstChild?.snapshot.routeConfig?.path ?? ''));
  }
}
