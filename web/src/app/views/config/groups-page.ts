import { Component, computed, inject } from '@angular/core';
import { ActivatedRoute } from '@angular/router';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { configPage } from './pages';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

/** The pages that are nothing but their groups. Which groups those are comes
 *  from the table keyed by the route's own path, so these three routes share one
 *  component and a fourth would need no code at all. */
@Component({
  selector: 'app-config-groups',
  imports: [ConfigGroupCard, ConfigSaveBar],
  templateUrl: './groups-page.html',
  styleUrl: './page.css',
  providers: [ConfigForm],
})
export class ConfigGroupsPage {
  private readonly store = inject(ConfigStore);
  private readonly page = configPage(inject(ActivatedRoute).snapshot.routeConfig?.path ?? '');

  readonly groups = computed(() => this.store.groupsFor(this.page.path));
}
