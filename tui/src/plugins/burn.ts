import type { Plugin } from './types';

// /burn <goal>
//
// Drives the active group toward <goal>, scripting a continuation prompt
// every time the agent goes idle (no events for 10s), until the 5h Anthropic
// rate-limit budget is effectively exhausted (≥95% utilization, or rejected
// overage with ≥99%) — or until the user cancels via Ctrl+C / /stop.
//
// Idle detection note: nextEvent returns null only after a true 10s of silence
// from this group's stream. Because every send triggers a `prompt` echo back
// through the daemon's tail, the first event after each send is the plugin's
// own prompt — that resets the silence timer naturally, and the loop then
// waits for stream/done events to also dry up before declaring idle.

const IDLE_TIMEOUT_MS = 10_000;

const util5h = (m: any): number => {
  const v = m?.ratelimit?.['anthropic-ratelimit-unified-5h-utilization'];
  return v == null ? 0 : parseFloat(String(v));
};

const overageStatus = (m: any): string =>
  m?.ratelimit?.['anthropic-ratelimit-unified-overage-status'] ?? '?';

const continuation = (goal: string, iter: number): string =>
  `continue working on the goal. iteration ${iter}.\n\ngoal: ${goal}`;

export const burnPlugin: Plugin = {
  name: 'burn',
  desc: 'pump the agent on a goal until 5h budget is exhausted',
  async run(ctx) {
    const goal = ctx.args.trim();
    if (!goal) { ctx.log('usage: /burn <goal>', 'err'); return; }

    let iter = 1;
    ctx.log(`burn: iter ${iter} — goal="${goal}"`);
    await ctx.send(goal);

    while (!ctx.signal.aborted) {
      // Drain events until we get 10s of silence. Each event resets the
      // timer naturally because nextEvent() awaits the *next* one.
      while (!ctx.signal.aborted) {
        const ev = await ctx.nextEvent(IDLE_TIMEOUT_MS);
        if (ev === null) break;
      }
      if (ctx.signal.aborted) return;

      const m = await ctx.metrics();
      const u = util5h(m);
      const ov = overageStatus(m);
      ctx.log(`burn: idle — 5h=${(u * 100).toFixed(1)}% overage=${ov}`);

      if (u >= 0.95 || (ov === 'rejected' && u >= 0.99)) {
        ctx.log(`burn: stop — limit reached (5h=${(u * 100).toFixed(1)}%)`);
        return;
      }

      iter += 1;
      ctx.log(`burn: iter ${iter}`);
      await ctx.send(continuation(goal, iter));
    }
  },
};
