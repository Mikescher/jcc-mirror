import { Component, inject, signal } from '@angular/core';
import { Api } from '../../api';
import * as fmt from '../../format';
import type { AuditEntry } from '../../models';

/** What was changed. The trail is the reason nothing here is read from a file:
 *  every setting arrived through this dashboard and says when and by whom. */
@Component({
  selector: 'app-config-audit',
  imports: [],
  templateUrl: './audit.html',
  styleUrl: './page.css',
})
export class ConfigAuditPage {
  private readonly api = inject(Api);
  readonly fmt = fmt;

  readonly audit = signal<AuditEntry[]>([]);
  readonly error = signal('');

  constructor() {
    void this.load();
  }

  async load(): Promise<void> {
    try {
      this.audit.set((await this.api.audit(50)) ?? []);
    } catch (err) {
      this.error.set(err instanceof Error ? err.message : String(err));
    }
  }
}
