import { Injectable, signal } from '@angular/core';
import type {
  AuditEntry,
  BandwidthView,
  ChangeRow,
  ConfigEntry,
  DiagnosticsView,
  EventRow,
  Job,
  Pair,
  PairView,
  RemoteListing,
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

/** Api is the whole HTTP surface. Reading needs nothing; changing anything needs
 *  the token, which the browser holds as a SameSite=Strict cookie the daemon set
 *  - so nothing here ever handles it after the unlock. */
@Injectable({ providedIn: 'root' })
export class Api {
  /** Whether this browser holds the token. Everything that can change something
   *  is disabled while it is false; what actually enforces it is the daemon. */
  readonly authed = signal(false);

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
    if (res.status === 401) this.authed.set(false);

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

  // ---- session ----------------------------------------------------------

  async session(): Promise<boolean> {
    const { authed } = await this.get<{ authed: boolean }>('/api/session');
    this.authed.set(authed);
    return authed;
  }

  async login(token: string): Promise<void> {
    await this.post('/api/login', { token });
    this.authed.set(true);
  }

  async logout(): Promise<void> {
    await this.post('/api/logout');
    this.authed.set(false);
  }

  // ---- read views -------------------------------------------------------

  status = () => this.get<Status>('/api/status');
  schedule = () => this.get<ScheduleView>('/api/schedule');
  config = () => this.get<ConfigEntry[]>('/api/config');
  audit = (limit = 50) => this.get<AuditEntry[]>('/api/config/audit', { limit });
  pairs = () => this.get<PairView[]>('/api/pairs');
  runs = () => this.get<{ current?: Run; history?: Run[] }>('/api/runs');
  diagnostics = () => this.get<DiagnosticsView>('/api/diagnostics');
  trash = (pair?: number) => this.get<TrashView[]>('/api/trash', { pair });
  scans = (pair: number, limit = 20) => this.get<Scan[]>('/api/scans', { pair, limit });
  remoteList = (path: string) => this.get<RemoteListing>('/api/remote/list', { path });
  update = () => this.get<UpdateStatus>('/api/update');

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

  bandwidth = (span: string, hours?: number) =>
    this.get<BandwidthView>('/api/bandwidth', { span, hours });

  // ---- actions, all of which need the token -----------------------------

  setConfig = (values: Record<string, string>) =>
    this.post<{ changed: string[]; status: Status }>('/api/config', values);

  probeRemote = () => this.post<Record<string, unknown>>('/api/remote/probe');

  /** Ignores the per-event toggles: pressing it is the request. Without it a
   *  mistyped user key means notifications silently never arrive. */
  testNotification = () => this.post<{ sent: boolean }>('/api/notify/test');
  ping = (target?: string) =>
    this.post<{ target: string; millis: number }>('/api/diagnostics/ping', { target });
  throughput = (path: string, bytes?: number) =>
    this.post<{ path: string; bytes: number; millis: number }>('/api/diagnostics/throughput', {
      path,
      bytes,
    });

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
