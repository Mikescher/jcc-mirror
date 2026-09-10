/** The tabs of the config view, and the one place a settings group is told which
 *  tab it belongs on. The names are the registry's own (GET /api/config →
 *  group), so a key added on the Go side arrives here under a name that is
 *  either claimed below or rendered on the fallback page - it can never fall out
 *  of the dashboard unnoticed. */
export interface ConfigPage {
  path: string;
  label: string;
  /** The sentence under the tabs. It says what the page decides, not what the
   *  individual fields mean: those carry their own help. */
  blurb: string;
  groups: string[];
}

/** The page an unclaimed group is rendered on. */
export const fallbackPath = 'system';

export const configPages: readonly ConfigPage[] = [
  {
    path: 'connection',
    label: 'Connection',
    blurb:
      'The tunnel to the rootserver and the SMB share behind it. Nothing is mirrored until both of these answer.',
    groups: ['Tunnel', 'Remote'],
  },
  {
    path: 'schedule',
    label: 'Schedule',
    blurb:
      'When transfers and scans are allowed to run, and how fast. A closed window stops the scheduler, not a run you started by hand.',
    groups: ['Schedule'],
  },
  {
    path: 'pairs',
    label: 'Pairs',
    blurb:
      "One directory of the publisher's share mapped onto one directory here. Nothing is mirrored until a pair exists.",
    groups: [],
  },
  {
    path: 'transfer',
    label: 'Transfer',
    blurb: 'How the walk, the copy and the deletion guards behave once a window is open.',
    groups: ['Scan', 'Transfer', 'Deletion'],
  },
  {
    path: 'jcc',
    label: 'jCC',
    blurb:
      'The ClipCornDB pair type: which file is the database, and how its replacement is guarded.',
    groups: ['jCC'],
  },
  {
    path: 'notifications',
    label: 'Notifications',
    blurb: 'Where SimpleCloudNotify sends, and which events are worth sending.',
    groups: ['Notifications'],
  },
  {
    path: fallbackPath,
    label: 'System',
    blurb:
      'The self-updater, how long history is kept, and the zone every timestamp is written in.',
    groups: ['Update', 'Retention', 'General'],
  },
  {
    path: 'audit',
    label: 'Audit',
    blurb: 'Every configuration change, newest first.',
    groups: [],
  },
];

/** The page a route path belongs to. The path is the route's own, so a component
 *  serving several routes reads its page off the router rather than an input. */
export function configPage(path: string): ConfigPage {
  return configPages.find((p) => p.path === path) ?? configPages[0];
}
