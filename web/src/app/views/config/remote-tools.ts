import { Component, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import { Live } from '../../live';
import type { RemoteListing } from '../../models';
import * as fmt from '../../format';

/** The questions worth asking a remote beyond "does it list": how far away it
 *  is, how fast it reads, and what is actually in it. */
@Component({
  selector: 'config-remote-tools',
  imports: [],
  templateUrl: './remote-tools.html',
  styleUrls: ['./page.css', './tools.css', './remote-tools.css'],
})
export class ConfigRemoteTools {
  private readonly api = inject(Api);
  private readonly live = inject(Live);
  readonly fmt = fmt;
  readonly unlocked = this.api.unlocked;

  readonly remotes = computed(() => this.live.status()?.remotes ?? []);

  /** 0 leaves the choice to the daemon, which takes the first remote. */
  readonly remote = signal(0);

  readonly pingTarget = signal('');
  readonly pingResult = signal<{ target: string; millis: number } | undefined>(undefined);
  readonly pingError = signal('');
  readonly pinging = signal(false);

  readonly probePath = signal('');
  readonly probeBytes = signal('');
  readonly throughput = signal<{ path: string; bytes: number; millis: number } | undefined>(
    undefined,
  );
  readonly throughputError = signal('');
  readonly throughputBusy = signal(false);

  readonly path = signal('');
  readonly listing = signal<RemoteListing | undefined>(undefined);
  readonly listError = signal('');
  readonly listBusy = signal(false);

  readonly crumbs = computed(() => {
    const out: { name: string; path: string }[] = [];
    let prefix = '';
    for (const part of this.path().split('/')) {
      if (!part) continue;
      prefix = prefix ? `${prefix}/${part}` : part;
      out.push({ name: part, path: prefix });
    }
    return out;
  });

  /** A listing and a timing belong to the remote they were taken from. */
  chooseRemote(id: number): void {
    this.remote.set(id);
    this.path.set('');
    this.listing.set(undefined);
    this.listError.set('');
    this.throughput.set(undefined);
    this.throughputError.set('');
  }

  async ping(): Promise<void> {
    this.pinging.set(true);
    this.pingError.set('');
    try {
      this.pingResult.set(
        await this.api.ping(this.pingTarget().trim(), this.remote() || undefined),
      );
    } catch (err) {
      this.pingResult.set(undefined);
      this.pingError.set(message(err));
    } finally {
      this.pinging.set(false);
    }
  }

  async measure(): Promise<void> {
    this.throughputBusy.set(true);
    this.throughputError.set('');
    try {
      const want = Number(this.probeBytes().trim());
      this.throughput.set(
        await this.api.throughput(
          this.probePath().trim(),
          want > 0 ? want : undefined,
          this.remote() || undefined,
        ),
      );
    } catch (err) {
      this.throughput.set(undefined);
      this.throughputError.set(message(err));
    } finally {
      this.throughputBusy.set(false);
    }
  }

  async open(path: string): Promise<void> {
    this.path.set(path);
    this.listBusy.set(true);
    this.listError.set('');
    try {
      this.listing.set(await this.api.remoteList(path, this.remote() || undefined));
    } catch (err) {
      this.listError.set(message(err));
    } finally {
      this.listBusy.set(false);
    }
  }
}

function message(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
