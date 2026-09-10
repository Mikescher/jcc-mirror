import { Component, inject } from '@angular/core';
import * as fmt from '../../format';
import { ConfigForm } from './form';

/** The save bar of one config page. It sticks to the bottom of the viewport
 *  while the page has unsaved changes, so Save is never a scroll away from the
 *  field that was just edited, and it stays out of the way entirely until there
 *  is something to save. */
@Component({
  selector: 'config-save-bar',
  imports: [],
  templateUrl: './save-bar.html',
  styleUrl: './save-bar.css',
})
export class ConfigSaveBar {
  readonly form = inject(ConfigForm);
  readonly fmt = fmt;
}
