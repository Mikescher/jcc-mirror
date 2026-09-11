import { Routes } from '@angular/router';

/** The six views of DESIGN.md §4, lazily loaded so the shell and the "now" view
 *  are what a first paint costs. Config is a view of its own tabs: which
 *  settings land on which of them is in views/config/pages.ts, not here. */
export const routes: Routes = [
  { path: '', pathMatch: 'full', redirectTo: 'now' },
  {
    path: 'now',
    title: 'Now · jcc-mirror',
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
    path: 'config',
    loadComponent: () => import('./views/config/shell').then((m) => m.ConfigShell),
    children: [
      { path: '', pathMatch: 'full', redirectTo: 'connection' },
      {
        path: 'connection',
        title: 'Connection · Config · jcc-mirror',
        loadComponent: () =>
          import('./views/config/connection').then((m) => m.ConfigConnectionPage),
      },
      {
        path: 'remotes',
        title: 'Remotes · Config · jcc-mirror',
        loadComponent: () => import('./views/config/remotes').then((m) => m.ConfigRemotesPage),
      },
      {
        path: 'schedule',
        title: 'Schedule · Config · jcc-mirror',
        loadComponent: () => import('./views/config/schedule').then((m) => m.ConfigSchedulePage),
      },
      {
        path: 'pairs',
        title: 'Pairs · Config · jcc-mirror',
        loadComponent: () => import('./views/config/pairs').then((m) => m.ConfigPairsPage),
      },
      {
        path: 'transfer',
        title: 'Transfer · Config · jcc-mirror',
        loadComponent: () => import('./views/config/groups-page').then((m) => m.ConfigGroupsPage),
      },
      {
        path: 'jcc',
        title: 'jCC · Config · jcc-mirror',
        loadComponent: () => import('./views/config/groups-page').then((m) => m.ConfigGroupsPage),
      },
      {
        path: 'notifications',
        title: 'Notifications · Config · jcc-mirror',
        loadComponent: () =>
          import('./views/config/notifications').then((m) => m.ConfigNotificationsPage),
      },
      {
        path: 'system',
        title: 'System · Config · jcc-mirror',
        loadComponent: () => import('./views/config/groups-page').then((m) => m.ConfigGroupsPage),
      },
      {
        path: 'audit',
        title: 'Audit · Config · jcc-mirror',
        loadComponent: () => import('./views/config/audit').then((m) => m.ConfigAuditPage),
      },
    ],
  },
  {
    path: 'diagnostics',
    title: 'Diagnostics · jcc-mirror',
    loadComponent: () => import('./views/diagnostics').then((m) => m.DiagnosticsPage),
  },
  { path: '**', redirectTo: 'now' },
];
