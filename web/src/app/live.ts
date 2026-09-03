import { Injectable, signal } from '@angular/core';
import type { ChangeRow, EventRow, LogLine, RunState, Status, StreamState } from './models';

/** How many of each kind the live buffers hold. The views page their own history
 *  from the REST endpoints; these are only what has happened since the page was
 *  opened. */
const bufferSize = 500;

/** Live is the one EventSource the whole dashboard shares. SSE rather than
 *  polling because it is one way and reconnects for free, and one connection
 *  rather than one per view because the daemon sends the same frames to all of
 *  them (DESIGN.md §4). */
@Injectable({ providedIn: 'root' })
export class Live {
  readonly connected = signal(false);
  readonly status = signal<Status | undefined>(undefined);
  readonly runs = signal<RunState>({});
  readonly events = signal<EventRow[]>([]);
  readonly changes = signal<ChangeRow[]>([]);
  readonly log = signal<LogLine[]>([]);

  private source?: EventSource;

  /** connect is idempotent, so every view may call it without knowing whether
   *  another one already has. */
  connect(): void {
    if (this.source) return;

    const source = new EventSource('/api/stream');
    this.source = source;

    source.onopen = () => this.connected.set(true);
    // EventSource retries by itself; all that is wanted here is to say so.
    source.onerror = () => this.connected.set(false);

    source.addEventListener('state', (e) => {
      const state = JSON.parse((e as MessageEvent<string>).data) as StreamState;
      this.status.set(state.status);
      this.runs.set(state.runs);
    });
    source.addEventListener('events', (e) => {
      this.events.update((rows) => prepend(rows, reverse(parse<EventRow>(e))));
    });
    source.addEventListener('changes', (e) => {
      this.changes.update((rows) => prepend(rows, reverse(parse<ChangeRow>(e))));
    });
    source.addEventListener('log', (e) => {
      this.log.update((lines) => append(lines, parse<LogLine>(e)));
    });
  }

  close(): void {
    this.source?.close();
    this.source = undefined;
    this.connected.set(false);
  }
}

function parse<T>(e: Event): T[] {
  return JSON.parse((e as MessageEvent<string>).data) as T[];
}

/** The stream sends rows oldest first; the tables read newest first. */
function reverse<T>(rows: T[]): T[] {
  return rows.slice().reverse();
}

function prepend<T>(existing: T[], incoming: T[]): T[] {
  return [...incoming, ...existing].slice(0, bufferSize);
}

function append<T>(existing: T[], incoming: T[]): T[] {
  return [...existing, ...incoming].slice(-bufferSize);
}
