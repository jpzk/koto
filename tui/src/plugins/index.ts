// Plugin registry. Add new plugins by importing and pushing them here.
// Discovery is intentionally explicit (not import.meta.glob) so the active
// surface is grep-able and tree-shakeable.

import type { Plugin } from './types';
import { burnPlugin } from './burn';

export const PLUGINS: Plugin[] = [burnPlugin];

export const PLUGIN_BY_NAME: Record<string, Plugin> =
  Object.fromEntries(PLUGINS.map(p => [p.name, p]));

export type { Event, Plugin, PluginCtx } from './types';
