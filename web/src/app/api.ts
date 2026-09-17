import { Injectable, signal } from '@angular/core';
import type {
  AuditEntry,
  BandwidthView,
  ChangeRow,
  ConfigEntry,
  DiagnosticsView,
  DryRunPage,
  EventRow,
  NotifyTarget,
  NotifyTopic,
  Job,
  Pair,
  PairView,
  Remote,
  RemoteListing,
  RemoteProbe,
  RemoteView,
  Restored,
  Run,
  Scan,
  ScheduleView,
  Status,
  TrashView,
  UpdateStatus,
} from './models';

/** ApiError carries the message the daemon wrote, which is meant to be read as
 *  it is: every handler answers `{"error": "..."}` with a sentence in it. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

/** unlockedKey is where the read-only toggle is remembered, so a reload does not
 *  silently re-arm every button on the page. */
const unlockedKey = 'jccmirror.unlocked';

/** Api is the whole HTTP surface. Every endpoint is behind the dashboard
 *  password, which the daemon checks per request; this app is only ever served
 *  to a browser that is already past it, so there is nothing to sign in to here
 *  and only the 401 of an expired cookie to handle. */
@Injectable({ providedIn: 'root' })
export class Api {
  /** Whether the actions are armed. This is a guard rail and nothing more - the
   *  daemon enforces nothing, so it stops a stray click rather than a stranger.
   *  It starts locked for that reason: read-only is the safe thing to land on. */
  readonly unlocked = signal(readUnlocked());

  async get<T>(path: string, params?: Record<string, string | number | undefined>): Promise<T> {
    const url = new URL(path, location.origin);
    for (const [key, value] of Object.entries(params ?? {})) {
      if (value !== undefined && value !== '') url.searchParams.set(key, String(value));
    }
    return this.request<T>(url.pathname + url.search, { method: 'GET' });
  }

  async post<T>(path: string, body?: Record<string, unknown>): Promise<T> {
    return this.request<T>(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body ?? {}),
    });
  }

  private async request<T>(path: string, init: RequestInit): Promise<T> {
    const res = await fetch(path, { ...init, credentials: 'same-origin' });

    // The cookie outlived the session it was made for, or the password was
    // changed under us. Reloading is the whole recovery: the daemon answers the
    // navigation with the password prompt instead of this app.
    if (res.status === 401) {
      location.reload();
      throw new ApiError(401, 'the dashboard password is needed again');
    }

    const text = await res.text();
    const parsed = text ? (JSON.parse(text) as unknown) : null;
    if (!res.ok) {
      const message =
        parsed && typeof parsed === 'object' && 'error' in parsed
          ? String((parsed as { error: unknown }).error)
          : `${res.status} ${res.statusText}`;
      throw new ApiError(res.status, message);
    }
    return parsed as T;
  }

  /** logout drops the session cookie. The reload is what shows the prompt: it is
   *  the daemon that decides this app is not served any more. */
  async logout(): Promise<void> {
    try {
      await this.post('/api/logout');
    } finally {
      location.reload();
    }
  }

  // ---- the read-only toggle ---------------------------------------------

  setUnlocked(v: boolean): void {
    this.unlocked.set(v);
    try {
      localStorage.setItem(unlockedKey, v ? '1' : '0');
    } catch {
      // A browser that refuses storage still gets the toggle, just not across
      // reloads.
    }
  }

  toggleUnlocked(): void {
    this.setUnlocked(!this.unlocked());
  }

  // ---- read views -------------------------------------------------------

  status = () => this.get<Status>('/api/status');
  schedule = () => this.get<ScheduleView>('/api/schedule');
  config = () => this.get<ConfigEntry[]>('/api/config');
  audit = (limit = 50) => this.get<AuditEntry[]>('/api/config/audit', { limit });
  pairs = () => this.get<PairView[]>('/api/pairs');
  remotes = () => this.get<RemoteView[]>('/api/remotes');
  runs = () => this.get<{ current?: Run; history?: Run[] }>('/api/runs');
  diagnostics = () => this.get<DiagnosticsView>('/api/diagnostics');
  trash = (pair?: number) => this.get<TrashView[]>('/api/trash', { pair });
  scans = (pair: number, limit = 20) => this.get<Scan[]>('/api/scans', { pair, limit });
  /** `remote` is a remote's id; left out, the daemon asks the first one. The
   *  same holds for the probe, the ping and the throughput read below. */
  remoteList = (path: string, remote?: number) =>
    this.get<RemoteListing>('/api/remote/list', { path, remote });
  update = () => this.get<UpdateStatus>('/api/update');
  notifyTopics = () => this.get<NotifyTopic[]>('/api/notify/topics');
  notifyTargets = () => this.get<NotifyTarget[]>('/api/notify/targets');

  events(filter: { limit?: number; kind?: string[]; level?: string[]; pair?: number } = {}) {
    const url = new URL('/api/events', location.origin);
    if (filter.limit) url.searchParams.set('limit', String(filter.limit));
    if (filter.pair) url.searchParams.set('pair', String(filter.pair));
    for (const k of filter.kind ?? []) url.searchParams.append('kind', k);
    for (const l of filter.level ?? []) url.searchParams.append('level', l);
    return this.request<EventRow[]>(url.pathname + url.search, { method: 'GET' });
  }

  changes(filter: { limit?: number; op?: string[]; pair?: number } = {}) {
    const url = new URL('/api/changes', location.origin);
    if (filter.limit) url.searchParams.set('limit', String(filter.limit));
    if (filter.pair) url.searchParams.set('pair', String(filter.pair));
    for (const op of filter.op ?? []) url.searchParams.append('op', op);
    return this.request<ChangeRow[]>(url.pathname + url.search, { method: 'GET' });
  }

  jobs(filter: { limit?: number; state?: string[]; pair?: number } = {}) {
    const url = new URL('/api/jobs', location.origin);
    if (filter.limit) url.searchParams.set('limit', String(filter.limit));
    if (filter.pair) url.searchParams.set('pair', String(filter.pair));
    for (const s of filter.state ?? []) url.searchParams.append('state', s);
    return this.request<Job[]>(url.pathname + url.search, { method: 'GET' });
  }

  dryRun(
    pair: number,
    filter: { op?: string[]; q?: string; offset?: number; limit?: number } = {},
  ) {
    const url = new URL('/api/runs/dryrun', location.origin);
    url.searchParams.set('pair', String(pair));
    if (filter.q) url.searchParams.set('q', filter.q);
    if (filter.offset) url.searchParams.set('offset', String(filter.offset));
    if (filter.limit) url.searchParams.set('limit', String(filter.limit));
    for (const op of filter.op ?? []) url.searchParams.append('op', op);
    return this.request<DryRunPage>(url.pathname + url.search, { method: 'GET' });
  }

  bandwidth = (span: string, hours?: number) =>
    this.get<BandwidthView>('/api/bandwidth', { span, hours });

  // ---- actions, all of which the toggle gates ---------------------------

  setConfig = (values: Record<string, string>) =>
    this.post<{ changed: string[]; status: Status }>('/api/config', values);

  /** Spreads a wg-quick file over the tunnel settings. `ignored` names the
   *  directives the file carried that a userspace tunnel cannot honour, which is
   *  worth showing rather than dropping quietly. */
  importWireguard = (config: string) =>
    this.post<{ changed: string[]; ignored?: string[]; status: Status }>(
      '/api/config/wireguard/import',
      { config },
    );

  probeRemote = (remote?: number) => this.post<RemoteProbe>('/api/remote/probe', { remote });

  /** Sends to that one target, ignoring its topics and whether it is enabled:
   *  pressing it is the request. Without it a mistyped user key means
   *  notifications silently never arrive. */
  testNotification = (id: number) => this.post<{ sent: boolean }>('/api/notify/test', { id });
  createNotifyTarget = (fields: Record<string, unknown>) =>
    this.post<NotifyTarget>('/api/notify/targets', fields);
  /** Sends only what changed. A blank or absent user key keeps the stored one;
   *  `events` replaces the whole set. */
  updateNotifyTarget = (fields: Record<string, unknown>) =>
    this.post<NotifyTarget>('/api/notify/targets/update', fields);
  deleteNotifyTarget = (id: number) =>
    this.post<NotifyTarget>('/api/notify/targets/delete', { id });
  /** An empty target pings the chosen remote's host. */
  ping = (target?: string, remote?: number) =>
    this.post<{ target: string; millis: number }>('/api/diagnostics/ping', { target, remote });
  throughput = (path: string, bytes?: number, remote?: number) =>
    this.post<{ path: string; bytes: number; millis: number }>('/api/diagnostics/throughput', {
      path,
      bytes,
      remote,
    });

  createRemote = (fields: Record<string, unknown>) => this.post<Remote>('/api/remotes', fields);
  /** Sends only what changed. A blank or absent password keeps the stored one;
   *  `clearPassword: true` is the only way to drop it. */
  updateRemote = (fields: Record<string, unknown>) =>
    this.post<Remote>('/api/remotes/update', fields);
  /** Refused with a 409 naming the pairs while any pair still reads from it. */
  deleteRemote = (id: number) => this.post<Remote>('/api/remotes/delete', { id });

  createPair = (fields: Record<string, unknown>) => this.post<Pair>('/api/pairs', fields);
  updatePair = (fields: Record<string, unknown>) => this.post<Pair>('/api/pairs/update', fields);
  deletePair = (id: number) => this.post<Pair>('/api/pairs/delete', { id });

  decideDeletion = (id: number, decision: 'approved' | 'rejected') =>
    this.post('/api/pairs/deletions', { id, decision });

  rollbackDatabase = (id: number, backup?: number, force?: boolean) =>
    this.post('/api/pairs/database/rollback', { id, backup, force });

  restoreTrash = (id: number, path: string, day?: string) =>
    this.post<Restored>('/api/trash/restore', { id, path, day });

  startRun = (kind: string, pair: number, force = false) =>
    this.post<Run>('/api/runs', { kind, pair, force });

  cancelRun = () => this.post('/api/runs/cancel');

  /** The self-updater's three buttons. Apply and rollback answer as soon as the
   *  binary is in place: the daemon then re-execs, so the next request from this
   *  page reaches the new process at the same address. */
  checkUpdate = () => this.post<UpdateStatus>('/api/update/check');
  applyUpdate = (force = false) => this.post('/api/update/apply', { force });
  rollbackUpdate = () => this.post('/api/update/rollback');

  /** Pausing is a setting rather than an endpoint of its own, so the pause lands
   *  in the audit trail beside every other change. The scheduler re-reads it
   *  every tick, so a run it started stops by itself. */
  setAutomatic = (on: boolean) => this.setConfig({ 'schedule.automatic': String(on) });
}

/** readUnlocked defaults to locked: anything unreadable, absent or written by an
 *  older build lands on read-only rather than on armed. */
function readUnlocked(): boolean {
  try {
    return localStorage.getItem(unlockedKey) === '1';
  } catch {
    return false;
  }
}
