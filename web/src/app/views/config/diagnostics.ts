import { Injectable, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import type { DiagnosticsView } from '../../models';

/** One fetch of GET /api/diagnostics shared by the pages that show a part of
 *  it. Refreshed on every arrival rather than cached for good: free space and
 *  the notification conditions move. */
@Injectable({ providedIn: 'root' })
export class DiagnosticsStore {
  private readonly api = inject(Api);

  readonly view = signal<DiagnosticsView | undefined>(undefined);
  readonly loading = signal(false);
  readonly error = signal('');

  /** A Go nil slice arrives as null. */
  readonly pairs = computed(() => this.view()?.pairs ?? []);
  readonly notify = computed(() => this.view()?.notify ?? []);

  async load(): Promise<void> {
    this.loading.set(true);
    this.error.set('');
    try {
      this.view.set(await this.api.diagnostics());
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
    } finally {
      this.loading.set(false);
    }
  }
}
