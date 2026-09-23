/** The JSON the daemon speaks, mirrored from the Go structs it is marshalled
 *  from. Times arrive as RFC 3339 strings and stay strings: nothing here does
 *  arithmetic on them that the daemon has not already done. */

export interface Status {
  version: string;
  buildStamp?: string;
  startedAt: string;
  uptimeSeconds: number;
  database: string;
  publicKey?: string;
  tunnel: TunnelStatus;
  remotes: RemoteStatus[];
  schedule: ScheduleStatus;
  /** The badge in the topbar. The rest of the update panel is a view of its own;
   *  this is the only part of it worth carrying on every live frame. */
  updateReady?: boolean;
}

export interface TunnelStatus {
  state: 'unconfigured' | 'failed' | 'connecting' | 'up';
  error?: string;
  endpoint?: string;
  addresses?: string[];
  listenPort?: number;
  peers?: Peer[];
}

export interface Peer {
  publicKey: string;
  endpoint?: string;
  handshakeAgeSeconds?: number;
  txBytes: number;
  rxBytes: number;
  allowedIps?: string[];
}

export interface RemoteStatus {
  id: number;
  name: string;
  target?: string;
  error?: string;
}

export interface Window {
  open: boolean;
  /** Bytes per second; 0 is no cap rather than no bandwidth. */
  limit: number;
  until: string;
}

export interface ScheduleStatus {
  timezone: string;
  now: string;
  automatic: boolean;
  scanEvery: string;
  transfer: Window;
  scan: Window;
  nextOpen?: string;
  error?: string;
}

export interface Cell {
  open: boolean;
  limit: number;
}

export interface GridView {
  rules: string;
  cells: Cell[][];
  days: string[];
}

export interface ScheduleView {
  now: ScheduleStatus;
  transfer: GridView;
  scan: GridView;
}

export interface ConfigEntry {
  key: string;
  group: string;
  label: string;
  help?: string;
  value?: string;
  secret?: boolean;
  required?: boolean;
  set: boolean;
}

export interface AuditEntry {
  id: number;
  ts: string;
  key: string;
  old: string;
  new: string;
  actor: string;
}

export type Level = 'debug' | 'info' | 'warn' | 'error';

export interface EventRow {
  id: number;
  ts: string;
  level: Level;
  kind: string;
  pairId?: number;
  message: string;
  data?: Record<string, unknown>;
}

export type ChangeOp = 'add' | 'replace' | 'delete' | 'fail';

export interface ChangeRow {
  id: number;
  ts: string;
  pairId?: number;
  path: string;
  op: ChangeOp;
  sizeBefore?: number;
  sizeAfter?: number;
  error?: string;
}

export interface Pair {
  id: number;
  name: string;
  type: 'raw' | 'jcc';
  /** 0 is none: nothing runs for the pair until one is chosen. */
  remoteId: number;
  /** Relative to the root of the pair's remote. */
  remotePath: string;
  localPath: string;
  mode: 'mirror' | 'guarded' | 'additive';
  includes: string[];
  excludes: string[];
  deleteGuard: number;
  priority: number;
  /** Numeric uid:gid for what the mirror creates; '' keeps the process's own. */
  owner: string;
  /** Octal, e.g. '0000'; '' is 0644 / 0755 minus the umask. */
  fileMode: string;
  dirMode: string;
  enabled: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface JobQueue {
  counts: Record<string, number>;
  pendingBytes: number;
  doneBytes: number;
  nextAttempt?: string;
}

export interface Space {
  path: string;
  free: number;
  total: number;
}

export interface DeleteApproval {
  id: number;
  pairId: number;
  scanId: number;
  files: number;
  bytes: number;
  reason: string;
  state: 'pending' | 'approved' | 'used' | 'rejected';
  createdAt: string;
  decidedAt?: string;
  decidedBy?: string;
  usedAt?: string;
}

export interface DBBackup {
  id: number;
  pairId: number;
  ts: string;
  path: string;
  relDb: string;
  size: number;
  mtime: string;
  hash: string;
  reason: 'replace' | 'rollback';
}

export interface PairView extends Pair {
  remoteName?: string;
  RemoteFiles: number;
  RemoteBytes: number;
  LocalFiles: number;
  LocalBytes: number;
  /** What the next sync would transfer, queued or not: new and changed files. */
  BehindFiles: number;
  BehindBytes: number;
  Queue: JobQueue;
  LastScan?: string;
  Space: Space;
  Approval?: DeleteApproval;
  Backups?: DBBackup[];
}

export interface Scan {
  id: number;
  pairId: number;
  startedAt: string;
  finishedAt?: string;
  dirs: number;
  files: number;
  bytes: number;
  state: 'running' | 'interrupted' | 'done' | 'failed';
  error?: string;
}

export interface Job {
  id: number;
  pairId: number;
  path: string;
  op: 'add' | 'replace' | 'delete';
  bytesTotal: number;
  bytesDone: number;
  mtime: string;
  state: 'pending' | 'running' | 'verifying' | 'done' | 'failed';
  attempts: number;
  nextAttemptAt?: string;
  error?: string;
  createdAt: string;
  updatedAt: string;
}

export interface PlanEntry {
  path: string;
  op: string;
  size: number;
  sizeBefore?: number;
  mtime: string;
}

export interface Plan {
  pairId: number;
  pair: string;
  scannedAt: string;
  add: number;
  addBytes: number;
  replace: number;
  replaceBytes: number;
  vanished: number;
  vanishedBytes: number;
  mode: string;
  guard?: string;
  approved?: boolean;
  excluded: number;
  database?: PlanEntry;
  remoteFiles: number;
  remoteBytes: number;
  localFiles: number;
  localBytes: number;
  free: number;
  reserve: number;
  shortfall: number;
  entries?: PlanEntry[];
  truncated?: boolean;
  queued: number;
}

export interface LockState {
  side: 'publisher' | 'here';
  path: string;
  held: boolean;
  mtime?: string;
  age?: number;
  stale?: boolean;
}

export interface DBResult {
  pairId: number;
  pair: string;
  path: string;
  replaced: boolean;
  bytes: number;
  mtime?: string;
  backup?: DBBackup;
  locks?: LockState[];
  forced?: boolean;
  deferred?: string;
  skipped?: string;
  duration: number;
}

export type RunKind = 'plan' | 'scan' | 'adopt' | 'sync' | 'delete' | 'db' | 'dryrun';

export interface Run {
  kind: RunKind;
  auto?: boolean;
  force?: boolean;
  /** Only a dry run has phases: the walk, then the diff against it. */
  phase?: 'walking' | 'diffing';
  pairId: number;
  pair: string;
  startedAt: string;
  finishedAt?: string;
  summary?: string;
  error?: string;
  plan?: Plan;
  database?: DBResult;
}

/** One page of a pair's latest dry run. `total` counts what the filter matched. */
export interface DryRunPage {
  pairId: number;
  pair: string;
  startedAt: string;
  finishedAt: string;
  plan: Plan;
  total: number;
  offset: number;
  entries: PlanEntry[];
}

export interface Progress {
  pair?: string;
  path?: string;
  fileDone: number;
  fileTotal: number;
  runBytes: number;
  runTotal: number;
  files: number;
  filesTotal: number;
  startedAt?: string;
}

export interface RunState {
  current?: Run;
  progress?: Progress;
  scan?: Scan;
  history?: Run[];
}

export interface StreamState {
  status: Status;
  runs: RunState;
}

export interface BWSample {
  ts: string;
  in: number;
  out: number;
}

export interface BandwidthView {
  span: 'minute' | 'hour' | 'day';
  timezone: string;
  from: string;
  to: string;
  samples: BWSample[];
  totalIn: number;
  totalOut: number;
  /** [weekday][hour] bytes in, Monday first. */
  heatmap: number[][];
  live: { in: number; out: number };
}

export interface LogLine {
  seq: number;
  ts: string;
  level: Level;
  text: string;
}

export interface NotifyState {
  kind: string;
  pairId: number;
  active: boolean;
  since: string;
  notifiedAt?: string;
  detail?: string;
}

/** One SimpleCloudNotifier account messages go to. The user key never leaves
 *  the daemon; userKeySet is all the page learns. */
export interface NotifyTarget {
  id: number;
  name: string;
  userId: string;
  userKeySet: boolean;
  channel: string;
  sender: string;
  enabled: boolean;
  /** Topic keys, see NotifyTopic. */
  events: string[];
  createdAt: string;
  updatedAt: string;
}

/** A kind of message a target can be told about. */
export interface NotifyTopic {
  key: string;
  label: string;
  help: string;
  default: boolean;
}

export interface PairDiagnostics {
  id: number;
  name: string;
  type: string;
  localPath: string;
  space: Space;
  spaceError?: string;
}

export interface DiagnosticsView {
  status: Status;
  pairs: PairDiagnostics[];
  notify: NotifyState[];
  nonNfcNames: number;
  dataDir: string;
  log: LogLine[];
}

/** UpdateStatus is the self-updater panel: what is running, what the URL has,
 *  and which of the two buttons makes sense right now (DESIGN.md §5). */
export interface UpdateStatus {
  version: string;
  buildStamp?: string;
  builtAt?: string;
  binary?: string;
  /** Whether the running binary came out of the data volume rather than the
   *  image, which is what a first install runs. */
  managed: boolean;
  configured: boolean;
  source?: string;
  auto: boolean;
  every?: string;
  checkedAt?: string;
  available?: Release;
  newer: boolean;
  /** Set when the binary at the URL is one a previous attempt rolled back, and
   *  will therefore not be installed again on its own. */
  blocked?: string;
  busy?: boolean;
  error?: string;
  state?: UpdateState;
  canRollBack: boolean;
}

export interface Release {
  url: string;
  modTime: string;
  size: number;
}

export interface UpdateState {
  state: 'applied' | 'confirmed' | 'rolledback';
  updatedAt: string;
  source?: string;
  remoteModTime: string;
  remoteSize?: number;
  fromVersion?: string;
  toVersion?: string;
  toBuild?: string;
  rolledBackAt?: string;
  rollbackReason?: string;
}

/** One SMB share of the publisher's, rooted at a directory inside it. The
 *  password never leaves the daemon; passwordSet is all the page learns. */
export interface Remote {
  id: number;
  name: string;
  host: string;
  share: string;
  path: string;
  user: string;
  passwordSet: boolean;
  domain: string;
  createdAt: string;
  updatedAt: string;
}

export interface RemoteView extends Remote {
  /** Empty while the daemon holds no client for it. */
  target: string;
  error?: string;
  /** The names of the pairs reading from it. */
  pairs: string[];
}

export interface RemoteProbe {
  remote: number;
  name: string;
  target: string;
  dirs: number;
  files: number;
  millis: number;
}

export interface RemoteEntry {
  path: string;
  name: string;
  size: number;
  mtime: string;
  isDir: boolean;
}

export interface RemoteListing {
  remote: number;
  name: string;
  path: string;
  target: string;
  millis: number;
  entries: RemoteEntry[];
}

export interface TrashDay {
  day: string;
  path: string;
  files: number;
  bytes: number;
  expires: string;
}

export interface TrashView {
  pairId: number;
  pair: string;
  days: TrashDay[];
}

export interface Restored {
  path: string;
  from: string;
  size: number;
}
