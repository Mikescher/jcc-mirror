import { Route, Routes } from '@angular/router';
import { configPages } from './views/config/pages';

type LoadComponent = NonNullable<Route['loadComponent']>;

/** The settings pages each own a component; the ones that are nothing but their
 *  settings groups share ConfigGroupsPage, which reads its groups off the route
 *  path. */
const configComponents: Record<string, LoadComponent> = {
  connection: () => import('./views/config/connection').then((m) => m.ConfigConnectionPage),
  remotes: () => import('./views/config/remotes').then((m) => m.ConfigRemotesPage),
  schedule: () => import('./views/config/schedule').then((m) => m.ConfigSchedulePage),
  pairs: () => import('./views/config/pairs').then((m) => m.ConfigPairsPage),
  notifications: () =>
    import('./views/config/notifications').then((m) => m.ConfigNotificationsPage),
  update: () => import('./views/config/update').then((m) => m.ConfigUpdatePage),
  audit: () => import('./views/config/audit').then((m) => m.ConfigAuditPage),
};

const groupsPage: LoadComponent = () =>
  import('./views/config/groups-page').then((m) => m.ConfigGroupsPage);

/** The four live views of DESIGN.md §4 and the settings pages, all in the one
 *  tab bar. Lazily loaded so the shell and the "now" view are what a first
 *  paint costs. Which settings land on which page is in views/config/pages.ts. */
export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'now' },
  {
    path: 'now',
    title: 'Dashboard · jcc-mirror',
    loadComponent: () => import('./views/now').then((m) => m.NowPage),
  },
  {
    path: 'changes',
    title: 'Changes · jcc-mirror',
    loadComponent: () => import('./views/changes').then((m) => m.ChangesPage),
  },
  {
    path: 'events',
    title: 'Events · jcc-mirror',
    loadComponent: () => import('./views/events').then((m) => m.EventsPage),
  },
  {
    path: 'bandwidth',
    title: 'Bandwidth · jcc-mirror',
    loadComponent: () => import('./views/bandwidth').then((m) => m.BandwidthPage),
  },
  {
    path: '',
    loadComponent: () => import('./views/config/shell').then((m) => m.ConfigShell),
    children: configPages.map((page) => ({
      path: page.path,
      title: `${page.label} · jcc-mirror`,
      loadComponent: configComponents[page.path] ?? groupsPage,
    })),
  },
  { path: 'config', pathMatch: 'full', redirectTo: 'connection' },
  { path: 'config/:page', redirectTo: ':page' },
  { path: 'diagnostics', redirectTo: 'connection' },
  { path: '**', redirectTo: 'now' },
];
