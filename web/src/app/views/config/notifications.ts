import { Component, computed, inject, signal } from '@angular/core';
import { Api } from '../../api';
import * as fmt from '../../format';
import type { NotifyTarget, NotifyTopic } from '../../models';
import { DiagnosticsStore } from './diagnostics';
import { ConfigForm } from './form';
import { ConfigGroupCard } from './group';
import { ConfigSaveBar } from './save-bar';
import { ConfigStore } from './store';

interface TargetInput {
  key: string;
  label: string;
  placeholder?: string;
  help?: string;
}

const targetInputs: readonly TargetInput[] = [
  { key: 'name', label: 'Name', placeholder: 'phone' },
  {
    key: 'userId',
    label: 'SCN user id',
    help: 'One half of the SimpleCloudNotifier credentials.',
  },
  {
    key: 'userKey',
    label: 'SCN user key',
    help: 'The other half. It is a pair, not a single key.',
  },
  {
    key: 'channel',
    label: 'Channel',
    placeholder: 'optional',
    help: "Optional SCN channel, so the mirror's messages can be muted separately from everything else on the account.",
  },
  {
    key: 'sender',
    label: 'Sender name',
    placeholder: 'jcc-mirror',
    help: 'Which machine the message came from, when more than one thing sends to the same account.',
  },
];

/** One target as the edit form shows it. The key is never sent to the page, so
 *  its field starts blank, and blank is what keeps the stored one. Topics are
 *  comma-joined, which is also how the daemon reads an array. */
function targetFields(t: NotifyTarget): Record<string, string> {
  return {
    name: t.name,
    userId: t.userId,
    userKey: '',
    channel: t.channel,
    sender: t.sender,
    enabled: String(t.enabled),
    events: t.events.join(','),
  };
}

function toggle(events: string, key: string, on: boolean): string {
  const set = new Set(events.split(',').filter(Boolean));
  if (on) set.add(key);
  else set.delete(key);
  return [...set].join(',');
}

/** The SCN accounts messages go to, each with its own topics, the setting they
 *  share, and the conditions currently raised. */
@Component({
  selector: 'app-config-notifications',
  imports: [ConfigGroupCard, ConfigSaveBar],
  templateUrl: './notifications.html',
  styleUrls: ['./page.css', './record.css', './notifications.css'],
  providers: [ConfigForm],
})
export class ConfigNotificationsPage {
  private readonly api = inject(Api);
  private readonly store = inject(ConfigStore);
  readonly diagnostics = inject(DiagnosticsStore);
  readonly fmt = fmt;
  readonly unlocked = this.api.unlocked;
  readonly inputs = targetInputs;

  readonly groups = computed(() => this.store.groupsFor('notifications'));

  readonly topics = signal<NotifyTopic[]>([]);
  readonly targets = signal<NotifyTarget[]>([]);
  readonly error = signal('');

  readonly edits = signal<Record<number, Record<string, string>>>({});
  readonly newTarget = signal<Record<string, string>>({});
  readonly saving = signal(0);
  readonly adding = signal(false);
  /** The target whose removal is waiting to be confirmed; 0 for none. */
  readonly removing = signal(0);

  readonly testing = signal(0);
  readonly tests = signal<Record<number, { sent?: boolean; error?: string }>>({});

  constructor() {
    void this.diagnostics.load();
    void this.load();
  }

  async load(): Promise<void> {
    try {
      const [topics, targets] = await Promise.all([
        this.api.notifyTopics(),
        this.api.notifyTargets(),
      ]);
      this.topics.set(topics ?? []);
      this.targets.set((targets ?? []).map((t) => ({ ...t, events: t.events ?? [] })));
      if (!Object.keys(this.newTarget()).length) this.resetNew();
    } catch (err) {
      this.fail(err);
    }
  }

  topicLabels(t: NotifyTarget): string {
    const labels = new Map(this.topics().map((p) => [p.key, p.label]));
    return t.events.map((e) => labels.get(e) ?? e).join(', ');
  }

  pairName(id: number): string {
    return this.diagnostics.pairs().find((p) => p.id === id)?.name ?? '—';
  }

  // ---- existing targets ---------------------------------------------------

  field(t: NotifyTarget, key: string): string {
    return this.edits()[t.id]?.[key] ?? targetFields(t)[key] ?? '';
  }

  hasTopic(t: NotifyTarget, key: string): boolean {
    return this.field(t, 'events').split(',').includes(key);
  }

  dirty(id: number): boolean {
    return Object.keys(this.edits()[id] ?? {}).length > 0;
  }

  /** An edit back to the stored value is no edit, so a key typed and then
   *  erased does not leave Save armed with nothing to send. */
  edit(t: NotifyTarget, key: string, value: string): void {
    const stored = targetFields(t);
    if (key === 'events') value = orderTopics(value, this.topics());
    this.edits.update((edits) => {
      const fields = { ...edits[t.id], [key]: value };
      if (value === stored[key]) delete fields[key];
      return { ...edits, [t.id]: fields };
    });
  }

  editTopic(t: NotifyTarget, key: string, on: boolean): void {
    this.edit(t, 'events', toggle(this.field(t, 'events'), key, on));
  }

  revert(id: number): void {
    this.edits.update((edits) => {
      const next = { ...edits };
      delete next[id];
      return next;
    });
  }

  async save(t: NotifyTarget): Promise<void> {
    const fields = this.edits()[t.id];
    if (!fields || !Object.keys(fields).length) return;

    this.saving.set(t.id);
    this.error.set('');
    try {
      const { events, ...rest } = fields;
      await this.api.updateNotifyTarget({
        id: t.id,
        ...rest,
        ...(events !== undefined ? { events: events.split(',').filter(Boolean) } : {}),
      });
      this.revert(t.id);
      await this.load();
    } catch (err) {
      this.fail(err);
    } finally {
      this.saving.set(0);
    }
  }

  async remove(t: NotifyTarget): Promise<void> {
    this.error.set('');
    try {
      await this.api.deleteNotifyTarget(t.id);
      this.removing.set(0);
      await this.load();
    } catch (err) {
      this.fail(err);
    }
  }

  async test(t: NotifyTarget): Promise<void> {
    this.testing.set(t.id);
    try {
      await this.api.testNotification(t.id);
      this.tests.update((tests) => ({ ...tests, [t.id]: { sent: true } }));
    } catch (err) {
      const error = err instanceof Error ? err.message : String(err);
      this.tests.update((tests) => ({ ...tests, [t.id]: { error } }));
    } finally {
      this.testing.set(0);
    }
  }

  // ---- a new target -------------------------------------------------------

  newHasTopic(key: string): boolean {
    return (this.newTarget()['events'] ?? '').split(',').includes(key);
  }

  editNew(key: string, value: string): void {
    this.newTarget.update((fields) => ({ ...fields, [key]: value }));
  }

  editNewTopic(key: string, on: boolean): void {
    const events = orderTopics(toggle(this.newTarget()['events'] ?? '', key, on), this.topics());
    this.editNew('events', events);
  }

  async create(): Promise<void> {
    this.adding.set(true);
    this.error.set('');
    try {
      const { events, ...rest } = this.newTarget();
      await this.api.createNotifyTarget({
        ...rest,
        enabled: true,
        events: (events ?? '').split(',').filter(Boolean),
      });
      this.resetNew();
      await this.load();
    } catch (err) {
      this.fail(err);
    } finally {
      this.adding.set(false);
    }
  }

  private resetNew(): void {
    this.newTarget.set({
      name: '',
      userId: '',
      userKey: '',
      channel: '',
      sender: '',
      events: this.topics()
        .filter((t) => t.default)
        .map((t) => t.key)
        .join(','),
    });
  }

  private fail(err: unknown): void {
    this.error.set(err instanceof Error ? err.message : String(err));
  }
}

/** Topics in the daemon's order, so the same set always reads the same and an
 *  edit back to it compares equal. */
function orderTopics(events: string, topics: NotifyTopic[]): string {
  const set = new Set(events.split(',').filter(Boolean));
  return topics
    .map((t) => t.key)
    .filter((k) => set.has(k))
    .join(',');
}
