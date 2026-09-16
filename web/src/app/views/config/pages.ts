/** The settings pages of the tab bar, and the one place a settings group is told
 *  which page it belongs on. The names are the registry's own (GET /api/config →
 *  group), so a key added on the Go side arrives here under a name that is
 *  either claimed below or rendered on the fallback page - it can never fall out
 *  of the dashboard unnoticed. */
export interface ConfigPage {
  path: string;
  label: string;
  /** The sentence under the page title. It says what the page decides, not what
   *  the individual fields mean: those carry their own help. */
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
      'The WireGuard tunnel to the rootserver, which every remote is reached through, and how it is doing right now. Nothing is mirrored until it is up.',
    groups: ['Tunnel'],
  },
  {
    path: 'remotes',
    label: 'Remotes',
    blurb:
      "The publisher's SMB shares, reached through the tunnel, and the tools that ask them how they are doing. Each pair reads from one of them.",
    groups: [],
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
      'One directory of a remote mapped onto one directory here, with the free space it has and what a sync would do. Nothing is mirrored until a pair exists.',
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
    blurb:
      'The SimpleCloudNotifier accounts messages go to, which events each one hears about, and the conditions currently raised.',
    groups: ['Notifications'],
  },
  {
    path: 'update',
    label: 'Update',
    blurb:
      'Where a newer binary is fetched from, whether it is installed unattended, and the buttons that install or undo one.',
    groups: ['Update'],
  },
  {
    path: fallbackPath,
    label: 'System',
    blurb:
      'How long history is kept, the zone every timestamp is written in, and the daemon log. Every setting lives in the database, takes effect the moment it is saved, and is recorded under Audit.',
    groups: ['Retention', 'General'],
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
