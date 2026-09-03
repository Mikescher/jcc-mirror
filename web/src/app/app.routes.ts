import { Routes } from '@angular/router';

/** The six views of DESIGN.md §4, lazily loaded so the shell and the "now" view
 *  are what a first paint costs. */
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
    title: 'Config · jcc-mirror',
    loadComponent: () => import('./views/config').then((m) => m.ConfigPage),
  },
  {
    path: 'diagnostics',
    title: 'Diagnostics · jcc-mirror',
    loadComponent: () => import('./views/diagnostics').then((m) => m.DiagnosticsPage),
  },
  { path: '**', redirectTo: 'now' },
];
