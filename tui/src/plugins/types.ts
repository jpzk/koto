// Public contract every clawson TUI plugin implements.
//
// A plugin is invoked from the TUI's slash-command dispatch as `/<name> <args>`.
// Only one plugin can run at a time across all groups. The TUI builds a
// PluginCtx keyed to the active group at invocation time, then awaits run().

export type Event = {
  event: 'prompt' | 'stream' | 'done';
  group: string;
  msg?: string;
  text?: string;
  ts?: number;
  historical?: boolean;
};

export type PluginCtx = {
  group: string;                                                  // active group at invocation
  args: string;                                                   // raw text after the command
  signal: AbortSignal;                                            // observe for cancellation
  call: (cmd: string, extra?: Record<string, unknown>) => Promise<any>;
  send: (msg: string) => Promise<void>;                           // shorthand: call('send', {group, msg})
  metrics: () => Promise<any>;                                    // last metric for the group, or null
  log: (text: string, kind?: 'sys' | 'err') => void;              // adds a LogLine in the current group
  nextEvent: (timeoutMs: number) => Promise<Event | null>;        // null = timeout or aborted
  sleep: (ms: number) => Promise<void>;                           // resolves early on abort
};

export type Plugin = {
  name: string;                                                   // matches `/<name>`
  desc: string;                                                   // shown in /help
  run: (ctx: PluginCtx) => Promise<void>;
};
