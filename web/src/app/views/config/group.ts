import { Component, computed, inject, input } from '@angular/core';
import { Api } from '../../api';
import { ConfigForm, isBoolean } from './form';
import type { ConfigGroup } from './store';

/** One card of settings. It edits the draft of whichever page it is rendered on
 *  - the form is injected, not passed - so the fields of every tab behave the
 *  same and are written once. Anything a group needs beyond its fields is
 *  projected in and lands under them. */
@Component({
  selector: 'config-group',
  imports: [],
  templateUrl: './group.html',
  styleUrl: './group.css',
})
export class ConfigGroupCard {
  readonly group = input.required<ConfigGroup>();

  /** Keys the page already draws a field for itself. Two fields bound to one key
   *  on one screen disagree the moment either is typed in, and the grids on the
   *  Schedule page render their rules in canonical form, so the two would not
   *  even start out reading the same. */
  readonly except = input<readonly string[]>([]);

  readonly form = inject(ConfigForm);
  readonly unlocked = inject(Api).unlocked;
  readonly isBoolean = isBoolean;

  readonly entries = computed(() => {
    const skip = new Set(this.except());
    return this.group().entries.filter((e) => !skip.has(e.key));
  });
}
