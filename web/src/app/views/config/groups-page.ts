import { Component, computed, inject } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import * as fmt from '../../format';
import { DiagnosticsStore } from './diagnostics';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigLogTail } from './log-tail';
import { configPage } from './pages';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** The pages that are mostly their groups. Which groups those are comes from the
 *  table keyed by the route's own path, so these routes share one component and
 *  another would need no code at all. */
@Component({
  selector: 'app-config-groups',
  imports: [ConfigGroupCard, ConfigSaveBar, ConfigLogTail],
  templateUrl: './groups-page.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigGroupsPage {
  private readonly store = inject(ConfigStore);
  readonly diagnostics = inject(DiagnosticsStore);
  readonly fmt = fmt;
  readonly page = configPage(inject(ActivatedRoute).snapshot.routeConfig?.path ?? '');

  readonly groups = computed(() => this.store.groupsFor(this.page.path));

  constructor() {
    if (this.page.path === 'transfer' || this.page.path === 'system') {
      void this.diagnostics.load();
    }
  }
}
