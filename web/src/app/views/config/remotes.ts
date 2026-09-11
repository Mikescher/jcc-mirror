import { Component, computed, inject, signal } from '@angular/core';
import { RouterLink } from '@angular/router';
import { Api } from '../../api';
import { Live } from '../../live';
import * as fmt from '../../format';
import type { RemoteProbe, RemoteView } from '../../models';

interface RemoteInput {
  key: string;
  label: string;
  placeholder?: string;
  help?: string;
}

/** The fields of a remote, in the order both forms show them. */
const remoteInputs: readonly RemoteInput[] = [
  {
    key: 'name',
    label: 'Name',
    placeholder: 'nas',
    help: "What the pairs, the event log and the updater's Remote setting call it.",
  },
  {
    key: 'host',
    label: 'Host',
    placeholder: '10.13.13.2',
    help: "The publisher's SMB server, as host or host:port. It is reached through the tunnel, so this is his address inside it, e.g. 10.13.13.2. Port 445 unless it says otherwise.",
  },
  {
    key: 'share',
    label: 'Share',
    placeholder: 'media',
    help: 'The share to mount, named the way DSM names it - the name on its own, with no server prefix and no path.',
  },
  {
    key: 'path',
    label: 'Path in the share',
    help: "The directory inside the share that is this remote's root. Empty is the share itself; the publisher directory of every pair reading from it is relative to whichever this is.",
  },
  {
    key: 'user',
    label: 'Username',
    help: "A read-only account on the publisher's DSM. SMB has no usable anonymous mode, so this is required even for a share that is open to everyone.",
  },
  {
    key: 'password',
    label: 'Password',
    help: 'Stored in plaintext: it is a read-only account reached over a private tunnel.',
  },
  {
    key: 'domain',
    label: 'Domain',
    placeholder: 'optional',
    help: 'Optional NTLM domain or workgroup. An account local to the NAS - which is what a DSM user is - needs none.',
  },
];

/** What POST /api/remotes accepts, and what a new remote starts as. */
const remoteDefaults: Record<string, string> = {
  name: '',
  host: '',
  share: '',
  path: '',
  user: '',
  password: '',
  domain: '',
};

/** One remote as the edit form shows it. The password is never sent to the
 *  page, so its field starts blank, and blank is what keeps the stored one. */
function remoteFields(r: RemoteView): Record<string, string> {
  return {
    name: r.name,
    host: r.host,
    share: r.share,
    path: r.path,
    user: r.user,
    password: '',
    domain: r.domain,
    clearPassword: '',
  };
}

/** The remotes editor. Like a pair, a remote is a record of its own, saved one
 *  at a time, so this page has no save bar. */
@Component({
  selector: 'app-config-remotes',
  imports: [RouterLink],
  templateUrl: './remotes.html',
  styleUrls: ['./page.css', './record.css'],
})
export class ConfigRemotesPage {
  private readonly api = inject(Api);
  private readonly live = inject(Live);
  readonly fmt = fmt;
  readonly unlocked = this.api.unlocked;
  readonly inputs = remoteInputs;

  readonly remotes = signal<RemoteView[]>([]);
  readonly error = signal('');

  /** Each remote's own draft of only what differs from what is stored. */
  readonly remoteEdits = signal<Record<number, Record<string, string>>>({});
  readonly newRemote = signal<Record<string, string>>({ ...remoteDefaults });
  readonly savingRemote = signal(0);
  readonly adding = signal(false);
  /** The remote whose removal is waiting to be confirmed; 0 for none. */
  readonly removing = signal(0);

  readonly testing = signal(0);
  readonly tests = signal<Record<number, { probe?: RemoteProbe; error?: string }>>({});

  /** The live frame's view of each remote, which is fresher than the one the
   *  list was fetched with. */
  private readonly states = computed(
    () => new Map((this.live.status()?.remotes ?? []).map((s) => [s.id, s])),
  );

  constructor() {
    this.live.connect();
    void this.load();
  }

  async load(): Promise<void> {
    try {
      const remotes = (await this.api.remotes()) ?? [];
      // A Go nil slice arrives as null.
      this.remotes.set(remotes.map((r) => ({ ...r, pairs: r.pairs ?? [] })));
    } catch (err) {
      this.fail(err);
    }
  }

  targetOf(r: RemoteView): string {
    return this.states().get(r.id)?.target || r.target;
  }

  errorOf(r: RemoteView): string | undefined {
    const state = this.states().get(r.id);
    return state ? state.error : r.error;
  }

  remoteField(r: RemoteView, field: string): string {
    return this.remoteEdits()[r.id]?.[field] ?? remoteFields(r)[field] ?? '';
  }

  remoteDirty(id: number): boolean {
    return Object.keys(this.remoteEdits()[id] ?? {}).length > 0;
  }

  /** An edit back to the stored value is no edit, so a password typed and then
   *  erased does not leave Save armed with nothing to send. */
  editRemote(r: RemoteView, field: string, value: string): void {
    this.remoteEdits.update((edits) => {
      const fields = { ...edits[r.id], [field]: value };
      if (value === remoteFields(r)[field]) delete fields[field];
      return { ...edits, [r.id]: fields };
    });
  }

  clearPassword(r: RemoteView, on: boolean): void {
    this.editRemote(r, 'password', '');
    this.editRemote(r, 'clearPassword', on ? 'true' : '');
  }

  revertRemote(id: number): void {
    this.remoteEdits.update((edits) => {
      const next = { ...edits };
      delete next[id];
      return next;
    });
  }

  async saveRemote(r: RemoteView): Promise<void> {
    const fields = this.remoteEdits()[r.id];
    if (!fields || !Object.keys(fields).length) return;

    this.savingRemote.set(r.id);
    this.error.set('');
    try {
      const { clearPassword, ...changed } = fields;
      await this.api.updateRemote({
        id: r.id,
        ...changed,
        ...(clearPassword ? { clearPassword: true } : {}),
      });
      this.revertRemote(r.id);
      await this.load();
    } catch (err) {
      this.fail(err);
    } finally {
      this.savingRemote.set(0);
    }
  }

  editNew(field: string, value: string): void {
    this.newRemote.update((fields) => ({ ...fields, [field]: value }));
  }

  async createRemote(): Promise<void> {
    this.adding.set(true);
    this.error.set('');
    try {
      await this.api.createRemote(this.newRemote());
      this.newRemote.set({ ...remoteDefaults });
      await this.load();
    } catch (err) {
      this.fail(err);
    } finally {
      this.adding.set(false);
    }
  }

  async removeRemote(r: RemoteView): Promise<void> {
    this.error.set('');
    try {
      await this.api.deleteRemote(r.id);
      this.removing.set(0);
      await this.load();
    } catch (err) {
      this.fail(err);
    }
  }

  async test(r: RemoteView): Promise<void> {
    this.testing.set(r.id);
    try {
      const probe = await this.api.probeRemote(r.id);
      this.tests.update((tests) => ({ ...tests, [r.id]: { probe } }));
    } catch (err) {
      const error = err instanceof Error ? err.message : String(err);
      this.tests.update((tests) => ({ ...tests, [r.id]: { error } }));
    } finally {
      this.testing.set(0);
    }
  }

  private fail(err: unknown): void {
    this.error.set(err instanceof Error ? err.message : String(err));
  }
}
